package db

import (
	"context"
	"database/sql"

	"github.com/golang-sql/sqlexp"
)

// SQL Server batches run through go-mssqldb's message queue, which reports
// what a batch produces in order: result sets, row counts, PRINT and info
// messages, errors. So nothing is guessed from the batch's first word:
// "DECLARE …; SELECT …" shows its rows, and "UPDATE …" its rows affected.

// runMSSQL runs a batch and starts fetching its first result set with
// columns; without one, the result is the rows the batch affected.
func runMSSQL(ctx context.Context, conn *sql.Conn, query string, opts RunOptions) (*Result, error) {
	msgs := &sqlexp.ReturnMessage{}
	rows, err := conn.QueryContext(ctx, query, msgs)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	var batchErr error // the first error, reported once the batch is read
	for {
		switch m := msgs.Message(ctx).(type) {
		case sqlexp.MsgRowsAffected:
			res.RowsAffected += m.Count
			res.HasAffected = true
		case sqlexp.MsgNotice:
			res.Messages = append(res.Messages, m.Message.String())
		case sqlexp.MsgError:
			if batchErr == nil {
				batchErr = m.Error
			}
		case sqlexp.MsgNext:
			types, err := rows.ColumnTypes()
			if err == nil && len(types) > 0 && batchErr == nil {
				set := newRowSet(types, opts)
				set.drain = func() error { return drainMSSQL(ctx, rows, msgs) }
				go set.fetch(ctx, rows, opts.MemoryLimit, opts.Cancel)
				res.Rows = set
				return res, nil
			}
			for rows.Next() { // a result after an error: not shown
			}
		case sqlexp.MsgNextResultSet:
			if !rows.NextResultSet() {
				if err := rows.Err(); err != nil && batchErr == nil {
					batchErr = err
				}
				rows.Close()
				return res, batchErr
			}
		}
	}
}

// drainMSSQL reads the rest of a batch after the result set shown, so
// that its errors are reported and the session is left ready; the result
// sets it skips are a later feature. It returns the first error.
func drainMSSQL(ctx context.Context, rows *sql.Rows, msgs *sqlexp.ReturnMessage) error {
	var first error
	for {
		switch m := msgs.Message(ctx).(type) {
		case sqlexp.MsgError:
			if first == nil {
				first = m.Error
			}
		case sqlexp.MsgNext:
			for rows.Next() {
			}
		case sqlexp.MsgNextResultSet:
			if !rows.NextResultSet() {
				if err := rows.Err(); err != nil && first == nil {
					first = err
				}
				return first
			}
		}
	}
}
