package db

import (
	"context"
	"database/sql"
	"regexp"
	"time"
)

// fetchMySQL reads the rows shown, then the rest of the batch: its later
// statements run as their results are read, so an error among them is the
// fetch's, reported with the rows kept.
func fetchMySQL(ctx context.Context, set *RowSet, rows *sql.Rows, opts RunOptions) {
	start := time.Now()
	defer func() {
		rows.Close()
		set.finish(start)
		opts.Cancel()
	}()
	if set.fill(ctx, rows, opts.MemoryLimit) {
		return // at the memory limit: the rest is not read
	}
	if err := rows.Err(); err != nil {
		set.fail(ctx, err)
		return
	}
	for rows.NextResultSet() {
	}
	if err := rows.Err(); err != nil {
		set.fail(ctx, err)
	}
}

// mysqlLine finds the line of a syntax error in MySQL's message.
var mysqlLine = regexp.MustCompile(`at line (\d+)$`)

// runMySQL runs a batch as a query, whatever its first word: the first
// result set with columns is shown ("SET @n := 2; SELECT …" included);
// without one, the result is the rows the last statement affected, which
// the server keeps for ROW_COUNT().
func runMySQL(ctx context.Context, conn *sql.Conn, query string, opts RunOptions) (*Result, error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	for {
		types, err := rows.ColumnTypes()
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(types) > 0 {
			set := newRowSet(types, opts)
			go fetchMySQL(ctx, set, rows, opts)
			return &Result{Rows: set}, nil
		}
		if !rows.NextResultSet() {
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	res := &Result{}
	// -1 after a statement that returns rows; DDL leaves 0.
	var n int64
	if conn.QueryRowContext(ctx, "SELECT ROW_COUNT()").Scan(&n) == nil && n >= 0 {
		res.RowsAffected, res.HasAffected = n, true
	}
	return res, nil
}
