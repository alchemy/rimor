package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/golang-sql/sqlexp"
)

// What exporting results needs from the database side (see package
// export): names quoted in a dialect, the table a result comes from, and
// a query run again with its rows streamed rather than kept.

// QuoteName quotes a name for the driver's dialect when it needs it, as
// the explorer's scripts do.
func QuoteName(d Driver, name string) string { return quoter{d}.ident(name) }

// SQLTable is the result's table for exported INSERTs, quoted as the
// scripts quote names: customers rather than editing's "main"."customers".
// An attached SQLite database keeps its name (aux.t).
func (o *Origin) SQLTable() string {
	q := quoter{o.driver}
	if o.driver == SQLite && o.schema != "" && o.schema != "main" {
		return q.ident(o.schema) + "." + q.ident(o.name)
	}
	return q.qualified(o.schema, o.name)
}

// Name is the result's table, unqualified.
func (o *Origin) Name() string { return o.name }

// ExportColumn is a result column as an export writes it.
type ExportColumn struct {
	Name, Type string
	Numeric    bool
}

// ErrNoResult says the query returned no result set to export.
var ErrNoResult = errors.New("the query returned no rows to export")

// ExportQuery runs a query again on a session and hands its result set
// number set (counting only those with columns, from 0) to start and then,
// row by row, to row. Values are formatted as the grid shows them, except
// binary ones, which are whole. It stops at the first error from row, or
// when ctx ends.
func ExportQuery(ctx context.Context, s *sql.Conn, d Driver, query string, set int,
	start func([]ExportColumn) error, row func([]Value) error) error {
	if d == SQLServer {
		return exportMSSQL(ctx, s, query, set, start, row)
	}
	rows, err := s.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for n := 0; ; {
		types, err := rows.ColumnTypes()
		if err != nil {
			return err
		}
		if len(types) > 0 {
			if n == set {
				if err := exportRows(rows, types, start, row); err != nil {
					return err
				}
				// A batch's later statements run as their results are read
				// (MySQL): read them, so that an error among them counts.
				for rows.NextResultSet() {
				}
				return rows.Err()
			}
			n++
		}
		if !rows.NextResultSet() {
			if err := rows.Err(); err != nil {
				return err
			}
			return ErrNoResult
		}
	}
}

// exportMSSQL reads a SQL Server batch through the message queue, as
// runMSSQL does: a plain query does not step past a batch's first
// statement without rows (DECLARE …; SELECT …).
func exportMSSQL(ctx context.Context, s *sql.Conn, query string, set int,
	start func([]ExportColumn) error, row func([]Value) error) error {
	batches := splitGO(query)
	if len(batches) == 0 {
		batches = []sqlBatch{{Text: query}}
	}
	e := &mssqlExport{set: set, start: start, row: row}
	for _, b := range batches {
		if err := e.batch(ctx, s, b.Text); err != nil {
			return err
		}
	}
	switch {
	case e.err != nil:
		return e.err
	case !e.done:
		return ErrNoResult
	}
	return nil
}

// mssqlExport follows an export across a script's batches.
type mssqlExport struct {
	set   int
	n     int  // result sets with columns so far
	done  bool // the set was exported
	err   error
	start func([]ExportColumn) error
	row   func([]Value) error
}

// batch reads one batch, exporting the set when it comes. An error of the
// script before the set stops the export; after it, it still counts.
func (e *mssqlExport) batch(ctx context.Context, s *sql.Conn, text string) error {
	msgs := &sqlexp.ReturnMessage{}
	rows, err := s.QueryContext(ctx, text, msgs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for {
		switch m := msgs.Message(ctx).(type) {
		case sqlexp.MsgError:
			if e.err == nil {
				e.err = m.Error
			}
		case sqlexp.MsgNext:
			if types, err := rows.ColumnTypes(); err == nil && len(types) > 0 && !e.done {
				if e.n == e.set {
					if e.err != nil {
						return e.err
					}
					if err := exportRows(rows, types, e.start, e.row); err != nil {
						return err
					}
					e.done = true // read the rest: its errors count
					continue
				}
				e.n++
			}
			for rows.Next() {
			}
		case sqlexp.MsgNextResultSet:
			if !rows.NextResultSet() {
				return ctx.Err()
			}
		}
	}
}

func exportRows(rows *sql.Rows, types []*sql.ColumnType, start func([]ExportColumn) error, row func([]Value) error) error {
	cols := make([]ExportColumn, len(types))
	for i, t := range types {
		cols[i] = ExportColumn{Name: t.Name(), Type: t.DatabaseTypeName(), Numeric: numericType(t.DatabaseTypeName())}
	}
	if err := start(cols); err != nil {
		return err
	}
	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	values := make([]Value, len(cols))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i, v := range raw {
			values[i] = formatCell(v, cols[i].Type, 0)
		}
		if err := row(values); err != nil {
			return err
		}
	}
	return rows.Err()
}
