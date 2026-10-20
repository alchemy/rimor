package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-sql/sqlexp"
	mssql "github.com/microsoft/go-mssqldb"
)

// SQL Server batches run through go-mssqldb's message queue, which reports
// what a batch produces in order: result sets, row counts, PRINT and info
// messages, errors. So nothing is guessed from the batch's first word, and
// every result set and message is kept, as SQL Server Management Studio
// shows them.

// MessageKind tells a batch's messages apart.
type MessageKind int

const (
	MessageInfo  MessageKind = iota // PRINT, RAISERROR below 11, SET STATISTICS output
	MessageRows                     // a statement's row count
	MessageError                    // a statement failed
)

// Message is one entry of a batch's message log, in the order the server
// sent them.
type Message struct {
	Kind  MessageKind
	Text  string // Info and Error
	Count int64  // Rows
	// Result is the number of result sets before the message, so a log can
	// say which result a count or error follows.
	Result int
}

// maxMessages bounds a batch's log; a PRINT in a loop can send millions.
const maxMessages = 10_000

// Batch is a SQL Server batch being read in the background: its result
// sets, each a RowSet, and its messages. Updated signals any change.
type Batch struct {
	mu       sync.Mutex
	sets     []*RowSet
	messages []Message
	dropped  int   // messages past maxMessages
	err      error // the first error
	stopped  StopReason
	done     bool

	updated chan struct{}
	cancel  context.CancelFunc
}

// Sets are the result sets so far.
func (b *Batch) Sets() []*RowSet {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*RowSet(nil), b.sets...)
}

// Messages are the messages so far, and how many more were dropped.
func (b *Batch) Messages() ([]Message, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.messages...), b.dropped
}

// Err is the first error the batch reported.
func (b *Batch) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// Done reports that the batch was read to its end, or stopped.
func (b *Batch) Done() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done
}

// Stopped says why the batch was not read to its end, if it was not.
func (b *Batch) Stopped() StopReason {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

// Updated is signalled when rows, result sets or messages arrive, and when
// the batch ends.
func (b *Batch) Updated() <-chan struct{} { return b.updated }

// Stop ends reading, keeping what arrived.
func (b *Batch) Stop() {
	b.mu.Lock()
	if !b.done && b.stopped == StopNone {
		b.stopped = StopCancelled
	}
	sets := b.sets
	b.mu.Unlock()
	if n := len(sets); n > 0 {
		sets[n-1].Stop() // the set being read, if it is
	}
	b.cancel()
}

func (b *Batch) notify() {
	select {
	case b.updated <- struct{}{}:
	default:
	}
}

func (b *Batch) log(m Message) {
	b.mu.Lock()
	m.Result = len(b.sets)
	if len(b.messages) < maxMessages {
		b.messages = append(b.messages, m)
	} else {
		b.dropped++
	}
	b.mu.Unlock()
	b.notify()
}

// logError logs an error and keeps the first, as the driver gave it, for
// its line and position.
func (b *Batch) logError(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
	b.log(Message{Kind: MessageError, Text: errorText(err)})
}

func (b *Batch) add(s *RowSet) {
	s.batch = b
	b.mu.Lock()
	b.sets = append(b.sets, s)
	b.mu.Unlock()
	b.notify()
}

// batchReader reads a batch's message queue.
type batchReader struct {
	ctx   context.Context
	rows  *sql.Rows
	msgs  *sqlexp.ReturnMessage
	batch *Batch
	res   *Result
	opts  RunOptions
	err   error // the first error, as the driver gave it
}

// runMSSQL runs a batch until its first result set with columns, which it
// starts fetching with the rest of the batch; without one, the result is
// the rows the batch affected, and its first error.
func runMSSQL(ctx context.Context, conn *sql.Conn, query string, opts RunOptions) (*Result, error) {
	msgs := &sqlexp.ReturnMessage{}
	rows, err := conn.QueryContext(ctx, query, msgs)
	if err != nil {
		return nil, err
	}
	b := &Batch{updated: make(chan struct{}, 1), cancel: opts.Cancel}
	r := &batchReader{ctx: ctx, rows: rows, msgs: msgs, batch: b, res: &Result{Batch: b}, opts: opts}
	first := r.nextSet()
	if first == nil {
		rows.Close()
		r.end()
		return r.res, r.err
	}
	r.res.Rows = first
	go r.readAll(first)
	return r.res, nil
}

// nextSet reads messages until a result set with columns begins, which it
// adds to the batch, or the batch ends (nil).
func (r *batchReader) nextSet() *RowSet {
	for {
		switch m := r.msgs.Message(r.ctx).(type) {
		case sqlexp.MsgRowsAffected:
			r.res.RowsAffected += m.Count
			r.res.HasAffected = true
			r.batch.log(Message{Kind: MessageRows, Count: m.Count})
		case sqlexp.MsgNotice:
			r.batch.log(Message{Kind: MessageInfo, Text: m.Message.String()})
		case sqlexp.MsgError:
			if r.err == nil {
				r.err = m.Error
			}
			r.batch.logError(m.Error)
		case sqlexp.MsgNext:
			if types, err := r.rows.ColumnTypes(); err == nil && len(types) > 0 {
				set := newRowSet(types, r.opts)
				r.batch.add(set)
				return set
			}
			for r.rows.Next() {
			}
		case sqlexp.MsgNextResultSet:
			if !r.rows.NextResultSet() {
				if err := r.rows.Err(); err != nil && r.ctx.Err() == nil {
					r.batch.logError(err)
					if r.err == nil {
						r.err = err
					}
				}
				return nil
			}
		}
	}
}

// readAll fetches each result set in turn, from the first, within the
// memory limit of the whole batch, and reads the batch to its end.
func (r *batchReader) readAll(set *RowSet) {
	defer func() {
		r.rows.Close()
		r.end()
	}()
	budget := r.opts.MemoryLimit
	for set != nil {
		start := time.Now()
		limited := budget <= 0 || set.fill(r.ctx, r.rows, budget)
		if limited {
			set.mu.Lock()
			set.stopped = StopLimit
			set.mu.Unlock()
		} else if err := r.rows.Err(); err != nil {
			set.fail(r.ctx, err)
		}
		set.finish(start)
		budget -= set.Progress().Bytes
		switch {
		case limited:
			r.batch.mu.Lock()
			r.batch.stopped = StopLimit
			r.batch.mu.Unlock()
			r.batch.log(Message{Kind: MessageInfo, Text: "Stopped at the memory limit (result_memory_mb): the rest of the batch was not read."})
			return
		case r.ctx.Err() != nil:
			return
		}
		set = r.nextSet()
	}
}

// end marks the batch over and releases its context.
func (r *batchReader) end() {
	r.batch.mu.Lock()
	r.batch.done = true
	r.batch.mu.Unlock()
	r.batch.notify()
	r.opts.Cancel()
}

// errorText writes a SQL Server error as Management Studio does:
// "Msg 8134, Level 16, State 1, Line 2" and the message.
func errorText(err error) string {
	var ms mssql.Error
	if !errors.As(err, &ms) {
		return err.Error()
	}
	where := fmt.Sprintf("Line %d", ms.LineNo)
	if ms.ProcName != "" {
		where = "Procedure " + ms.ProcName + ", " + where
	}
	return fmt.Sprintf("Msg %d, Level %d, State %d, %s\n%s", ms.Number, ms.Class, ms.State, where, ms.Message)
}
