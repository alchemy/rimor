package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// RowLimit caps how many rows a result keeps.
const RowLimit = 1000

// Session returns a dedicated connection to a database, so that session
// state (USE, SET, temp tables, open transactions) carries over between
// statements run from the same tab.
//
// Give it back with Release.
func (p *Pool) Session(ctx context.Context, database string) (*sql.Conn, error) {
	conn, err := p.DB(ctx, database)
	if err != nil {
		return nil, err
	}
	s, err := conn.Conn(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.sessions[s] = true
	p.mu.Unlock()
	return s, nil
}

// Release closes a session from Session.
func (p *Pool) Release(s *sql.Conn) {
	p.mu.Lock()
	delete(p.sessions, s)
	p.mu.Unlock()
	s.Close()
}

// IsConnLost reports whether an error means the session's connection is
// gone and the statement never ran, so a fresh session may retry it.
func IsConnLost(err error) bool {
	return errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) ||
		(err != nil && strings.Contains(err.Error(), "database is closed"))
}

// Column describes a result column.
type Column struct {
	Name    string
	Type    string // database type name, e.g. VARCHAR
	Numeric bool   // values align right
}

// Value is a formatted cell.
type Value struct {
	Text string
	Null bool
}

// Result is the outcome of one statement.
type Result struct {
	Columns []Column
	Rows    [][]Value
	// Truncated is set when the statement returned more than RowLimit rows.
	Truncated bool

	// RowsAffected is set for statements that do not return rows, when the
	// driver reports it.
	RowsAffected int64
	HasAffected  bool

	Duration time.Duration
}

// HasRows reports whether the statement produced a result set.
func (r *Result) HasRows() bool { return len(r.Columns) > 0 }

// rowKeywords start statements that return rows.
var rowKeywords = map[string]bool{
	"SELECT": true, "WITH": true, "VALUES": true, "TABLE": true, "SHOW": true,
	"EXPLAIN": true, "PRAGMA": true, "DESCRIBE": true, "EXEC": true, "EXECUTE": true,
	"CALL": true, "FETCH": true,
}

var returningClause = regexp.MustCompile(`(?i)\b(RETURNING|OUTPUT)\b`)

// returnsRows guesses whether a statement produces rows. Running a
// mutating statement through Exec is what yields its affected-row count,
// and it must never run twice, so the choice is made up front.
func returnsRows(query string) bool {
	word := strings.ToUpper(firstWord(query))
	return rowKeywords[word] || returningClause.MatchString(query)
}

// firstWord skips leading space, comments and parentheses.
func firstWord(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n(")
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = s[i+2:]
				continue
			}
			return ""
		}
		break
	}
	end := strings.IndexFunc(s, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	if end < 0 {
		return s
	}
	return s[:end]
}

// Run executes one statement (or, where the driver allows, one batch) on a
// session connection.
func Run(ctx context.Context, conn *sql.Conn, query string) (*Result, error) {
	start := time.Now()
	res := &Result{}
	var err error
	if returnsRows(query) {
		err = runQuery(ctx, conn, query, res)
	} else {
		var r sql.Result
		if r, err = conn.ExecContext(ctx, query); err == nil {
			if n, aerr := r.RowsAffected(); aerr == nil {
				res.RowsAffected, res.HasAffected = n, true
			}
		}
	}
	res.Duration = time.Since(start)
	return res, err
}

func runQuery(ctx context.Context, conn *sql.Conn, query string, res *Result) error {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	// The first result set with columns is shown; batches may start with
	// row counts from SET or DML statements.
	for {
		cols, err := rows.ColumnTypes()
		if err != nil {
			return err
		}
		if len(cols) > 0 {
			return readRows(rows, cols, res)
		}
		if !rows.NextResultSet() {
			return rows.Err()
		}
	}
}

func readRows(rows *sql.Rows, cols []*sql.ColumnType, res *Result) error {
	res.Columns = make([]Column, len(cols))
	numeric := make([]bool, len(cols)) // every non-null value was a number
	for i, c := range cols {
		res.Columns[i] = Column{Name: c.Name(), Type: c.DatabaseTypeName()}
		numeric[i] = true
	}

	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if len(res.Rows) == RowLimit {
			res.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make([]Value, len(cols))
		for i, v := range raw {
			row[i] = formatValue(v, res.Columns[i].Type)
			switch v.(type) {
			case nil, int64, int32, int, float64, float32:
			default:
				numeric[i] = false
			}
		}
		res.Rows = append(res.Rows, row)
	}
	for i := range res.Columns {
		res.Columns[i].Numeric = numericType(res.Columns[i].Type) || (numeric[i] && len(res.Rows) > 0)
	}
	if res.Truncated {
		return nil // rows.Close discards the rest
	}
	return rows.Err()
}

func numericType(name string) bool {
	switch strings.ToUpper(name) {
	case "INT", "INT2", "INT4", "INT8", "INTEGER", "BIGINT", "SMALLINT", "TINYINT",
		"NUMERIC", "DECIMAL", "FLOAT", "FLOAT4", "FLOAT8", "REAL", "DOUBLE",
		"MONEY", "SMALLMONEY", "OID":
		return true
	}
	return false
}

func formatValue(v any, typ string) Value {
	switch v := v.(type) {
	case nil:
		return Value{Text: "NULL", Null: true}
	case []byte:
		if strings.EqualFold(typ, "UNIQUEIDENTIFIER") && len(v) == 16 {
			var u mssql.UniqueIdentifier
			if u.Scan(v) == nil {
				return Value{Text: u.String()}
			}
		}
		if utf8.Valid(v) {
			return Value{Text: string(v)}
		}
		const show = 32
		s := "0x" + strings.ToUpper(hex.EncodeToString(v[:min(len(v), show)]))
		if len(v) > show {
			s += "…"
		}
		return Value{Text: s}
	case string:
		return Value{Text: v}
	case time.Time:
		return Value{Text: formatTime(v, typ)}
	case float64:
		return Value{Text: formatFloat(v, 64)}
	case float32:
		return Value{Text: formatFloat(float64(v), 32)}
	case bool:
		return Value{Text: strconv.FormatBool(v)}
	}
	return Value{Text: fmt.Sprint(v)}
}

func formatFloat(f float64, bits int) string {
	if math.Abs(f) >= 1e15 || (f != 0 && math.Abs(f) < 1e-6) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	return strconv.FormatFloat(f, 'f', -1, bits)
}

func formatTime(t time.Time, typ string) string {
	switch strings.ToUpper(typ) {
	case "DATE":
		return t.Format("2006-01-02")
	case "TIME":
		return t.Format("15:04:05.999999")
	}
	layout := "2006-01-02 15:04:05"
	if t.Nanosecond() != 0 {
		layout += ".999999"
	}
	return t.Format(layout)
}

// Error is a statement failure in a driver-independent shape.
type Error struct {
	Message string
	Code    string // SQLSTATE for Postgres, error number for SQL Server
	Detail  string
	Hint    string
	// Position is the 1-based character offset of the error in the
	// statement, and Line its 1-based line; zero when unknown.
	Position int
	Line     int
}

// Describe extracts what the driver knows about a failed statement.
func Describe(err error) Error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return Error{Message: pg.Message, Code: pg.Code, Detail: pg.Detail, Hint: pg.Hint, Position: int(pg.Position)}
	}
	var ms mssql.Error
	if errors.As(err, &ms) {
		e := Error{Message: ms.Message, Code: strconv.Itoa(int(ms.Number)), Line: int(ms.LineNo)}
		if ms.ProcName != "" {
			e.Detail = "in " + ms.ProcName
		}
		// Earlier messages of the batch often explain the last one.
		for _, prev := range ms.All {
			if prev.Message != ms.Message {
				e.Detail = strings.TrimSpace(e.Detail + "\n" + prev.Message)
			}
		}
		return e
	}
	switch {
	case errors.Is(err, context.Canceled):
		return Error{Message: "Query cancelled."}
	case errors.Is(err, context.DeadlineExceeded):
		return Error{Message: "Query timed out."}
	}
	return Error{Message: err.Error()}
}
