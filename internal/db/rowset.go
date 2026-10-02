package db

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"
)

// Fetching tunables.
const (
	chunkRows     = 10_000                 // rows per chunk at most
	flushInterval = 100 * time.Millisecond // publish a partial chunk this often
)

// StopReason says why fetching ended before the last row.
type StopReason int

const (
	StopNone      StopReason = iota // all rows fetched
	StopLimit                       // the memory limit was reached
	StopCancelled                   // the user stopped it
)

// chunk holds rows compactly: every cell's text concatenated in data, with
// the end offset of each cell (row-major) and a bit per NULL cell. That is
// about 4 bytes of overhead per cell, against ~40 for a string per cell.
type chunk struct {
	data  []byte
	ends  []uint32
	nulls []uint64
	rows  int
}

func (c *chunk) add(v Value) {
	i := len(c.ends)
	if v.Null {
		for len(c.nulls) <= i/64 {
			c.nulls = append(c.nulls, 0)
		}
		c.nulls[i/64] |= 1 << (i % 64)
	} else {
		c.data = append(c.data, v.Text...)
	}
	c.ends = append(c.ends, uint32(len(c.data)))
}

func (c *chunk) cell(i int) Value {
	if i/64 < len(c.nulls) && c.nulls[i/64]&(1<<(i%64)) != 0 {
		return Value{Text: "NULL", Null: true}
	}
	start := uint32(0)
	if i > 0 {
		start = c.ends[i-1]
	}
	return Value{Text: string(c.data[start:c.ends[i]])}
}

func (c *chunk) size() int64 {
	return int64(cap(c.data) + 4*cap(c.ends) + 8*cap(c.nulls) + 64)
}

// RowSet is a result set being fetched in the background. Readers see the
// rows published so far; Updated signals when more arrive.
type RowSet struct {
	columns []Column

	mu      sync.RWMutex
	chunks  []*chunk
	starts  []int // first row of each chunk
	rows    int
	bytes   int64
	numeric []bool // every non-null value so far was a number
	seen    []bool // a non-null value was seen
	done    bool
	stopped StopReason
	err     error
	fetched time.Duration

	updated chan struct{}
	cancel  context.CancelFunc
}

// Columns describes the result columns; they do not change.
func (s *RowSet) Columns() []Column { return s.columns }

// Len is the number of rows fetched so far.
func (s *RowSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rows
}

// Cell returns a fetched cell.
func (s *RowSet) Cell(row, col int) Value {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := sort.Search(len(s.starts), func(i int) bool { return s.starts[i] > row }) - 1
	c := s.chunks[i]
	return c.cell((row-s.starts[i])*len(s.columns) + col)
}

// Numeric reports whether a column's values align right.
func (s *RowSet) Numeric(col int) bool {
	if numericType(s.columns[col].Type) {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.numeric[col] && s.seen[col]
}

// Progress is a snapshot of the fetch.
type Progress struct {
	Rows    int
	Bytes   int64
	Done    bool
	Stopped StopReason
	Err     error         // set when fetching failed part way
	Elapsed time.Duration // time spent fetching, once done
}

func (s *RowSet) Progress() Progress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Progress{Rows: s.rows, Bytes: s.bytes, Done: s.done, Stopped: s.stopped, Err: s.err, Elapsed: s.fetched}
}

// Updated is signalled after rows are published and when fetching ends.
func (s *RowSet) Updated() <-chan struct{} { return s.updated }

// Stop ends fetching, keeping the rows fetched so far.
func (s *RowSet) Stop() {
	s.mu.Lock()
	if !s.done && s.stopped == StopNone {
		s.stopped = StopCancelled
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *RowSet) notify() {
	select {
	case s.updated <- struct{}{}:
	default: // a signal is already pending
	}
}

// publish makes a chunk visible, with the column statistics so far.
func (s *RowSet) publish(c *chunk, numeric, seen []bool) {
	if c.rows == 0 {
		return
	}
	s.mu.Lock()
	copy(s.numeric, numeric)
	copy(s.seen, seen)
	s.chunks = append(s.chunks, c)
	s.starts = append(s.starts, s.rows)
	s.rows += c.rows
	s.bytes += c.size()
	s.mu.Unlock()
	s.notify()
}

// fetch reads rows until the end, the limit or cancellation, then closes
// rows and calls release.
func (s *RowSet) fetch(ctx context.Context, rows *sql.Rows, limit int64, release func()) {
	start := time.Now()
	defer func() {
		rows.Close()
		s.mu.Lock()
		s.done = true
		s.fetched = time.Since(start)
		s.mu.Unlock()
		release()
		s.notify()
	}()

	cols := len(s.columns)
	raw := make([]any, cols)
	ptrs := make([]any, cols)
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	cur, flushed := &chunk{}, time.Now()
	var published int64 // bytes in published chunks

	// Column statistics, kept here and copied on publish.
	numeric, seen := make([]bool, cols), make([]bool, cols)
	for i := range numeric {
		numeric[i] = true
	}

	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			s.fail(ctx, err)
			break
		}
		for i, v := range raw {
			cur.add(formatValue(v, s.columns[i].Type))
			switch v.(type) {
			case nil:
			case int64, int32, int, float64, float32:
				seen[i] = true
			default:
				numeric[i] = false
			}
		}
		cur.rows++

		if cur.rows >= chunkRows || time.Since(flushed) >= flushInterval {
			published += cur.size()
			s.publish(cur, numeric, seen)
			cur, flushed = &chunk{}, time.Now()
			if published >= limit {
				s.mu.Lock()
				s.stopped = StopLimit
				s.mu.Unlock()
				return
			}
		}
	}
	s.publish(cur, numeric, seen)
	if err := rows.Err(); err != nil {
		s.fail(ctx, err)
	}
}

// fail records a fetch error, unless it is the cancellation of a stop.
func (s *RowSet) fail(ctx context.Context, err error) {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		s.mu.Lock()
		if s.stopped == StopNone {
			s.stopped = StopCancelled
		}
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}
