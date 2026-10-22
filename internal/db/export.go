package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/golang-sql/sqlexp"
)

// What exporting results needs from the database side (see package
// export): names quoted in a dialect, the table a result comes from, and
// a query run again with its rows streamed rather than kept.

// QuoteName quotes a name for the driver's dialect when it needs it, as
// the explorer's scripts do.
func QuoteName(d Driver, name string) string { return quoter{d}.ident(name) }

// QualifiedName names an object with its schema in the driver's dialect.
func QualifiedName(d Driver, schema, name string) string { return quoter{d}.qualified(schema, name) }

// SQLTable is the result's table for exported INSERTs: its readable name
// quoted as the scripts quote names, so customers rather than editing's
// fully qualified "main"."customers".
func (o *Origin) SQLTable() string {
	if schema, table, ok := strings.Cut(o.Table, "."); ok {
		return QualifiedName(o.driver, schema, table)
	}
	return QualifiedName(o.driver, "", o.Table)
}

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
				return exportRows(rows, types, start, row)
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
	msgs := &sqlexp.ReturnMessage{}
	rows, err := s.QueryContext(ctx, query, msgs)
	if err != nil {
		return err
	}
	defer rows.Close()
	var batchErr error
	n, done := 0, false
	for {
		switch m := msgs.Message(ctx).(type) {
		case sqlexp.MsgError:
			if batchErr == nil {
				batchErr = m.Error
			}
		case sqlexp.MsgNext:
			if types, err := rows.ColumnTypes(); err == nil && len(types) > 0 && !done {
				if n == set {
					if batchErr != nil {
						return batchErr
					}
					if err := exportRows(rows, types, start, row); err != nil {
						return err
					}
					done = true // read the rest of the batch: its errors count
					continue
				}
				n++
			}
			for rows.Next() {
			}
		case sqlexp.MsgNextResultSet:
			if !rows.NextResultSet() {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if batchErr != nil {
					return batchErr
				}
				if !done {
					return ErrNoResult
				}
				return nil
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
