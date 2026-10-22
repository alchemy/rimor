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

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// DefaultMemoryLimit is how much memory a result's rows may take before
// fetching stops.
const DefaultMemoryLimit = 512 << 20

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

// Release ends a session from Session by closing its connection, which is
// never returned to the pool: it may hold an open transaction, temp tables
// or settings (SET, USE) that must not reach the pool's next user, and
// closing makes the server roll back the transaction at once rather than
// when the connection is next reused (or, with SQLite, not at all).
func (p *Pool) Release(s *sql.Conn) {
	p.mu.Lock()
	delete(p.sessions, s)
	p.mu.Unlock()
	CloseSession(s)
}

// CloseSession closes a session's connection instead of pooling it:
// database/sql closes a connection when Raw's function reports it bad.
// It waits for the session's open rows, like Close.
func CloseSession(s *sql.Conn) {
	err := s.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil && !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, sql.ErrConnDone) {
		s.Close()
	}
}

// IsConnLost reports whether an error means the session's connection is
// gone and the statement never ran, so a fresh session may retry it.
func IsConnLost(err error) bool {
	return errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, mysql.ErrInvalidConn) ||
		(err != nil && strings.Contains(err.Error(), "database is closed"))
}

// Column describes a result column; see RowSet.Numeric for alignment.
type Column struct {
	Name string
	Type string // database type name, e.g. VARCHAR
}

// Value is a formatted cell.
type Value struct {
	Text string
	Null bool
}

// Result is the outcome of one statement.
type Result struct {
	// Rows is the result set, still being fetched; nil for statements that
	// do not return rows.
	Rows *RowSet

	// RowsAffected is set for statements that do not return rows, when the
	// driver reports it.
	RowsAffected int64
	HasAffected  bool

	// Duration is the time until the statement returned: its first rows,
	// for queries.
	Duration time.Duration

	// Batch, for SQL Server, holds every result set of the batch (Rows is
	// the first) and its messages; nil elsewhere.
	Batch *Batch
}

// HasRows reports whether the statement produced a result set.
func (r *Result) HasRows() bool { return r.Rows != nil }

// Sets are the result sets so far: the batch's, or the one there is.
func (r *Result) Sets() []*RowSet {
	switch {
	case r.Batch != nil:
		return r.Batch.Sets()
	case r.Rows != nil:
		return []*RowSet{r.Rows}
	}
	return nil
}

// Fetching reports whether rows or messages are still arriving.
func (r *Result) Fetching() bool {
	switch {
	case r.Batch != nil:
		return !r.Batch.Done()
	case r.Rows != nil:
		return !r.Rows.Progress().Done
	}
	return false
}

// Stop ends fetching, keeping what arrived.
func (r *Result) Stop() {
	switch {
	case r.Batch != nil:
		r.Batch.Stop()
	case r.Rows != nil:
		r.Rows.Stop()
	}
}

// Updated is signalled while rows (or, for a batch, results and messages)
// arrive and when they end; nil when nothing is fetched.
func (r *Result) Updated() <-chan struct{} {
	switch {
	case r.Batch != nil:
		return r.Batch.Updated()
	case r.Rows != nil:
		return r.Rows.Updated()
	}
	return nil
}

// RunOptions control a statement's execution.
type RunOptions struct {
	// MemoryLimit stops fetching once the rows take this many bytes;
	// zero means DefaultMemoryLimit.
	MemoryLimit int64
	// Cancel cancels the context passed to Run. Fetching outlives Run, so
	// it is called when fetching ends, and RowSet.Stop calls it.
	Cancel context.CancelFunc
	// CheckReadOnly refuses statements that may write (see CheckReadOnly),
	// for read-only connections whose database cannot enforce it.
	CheckReadOnly bool
	// Driver picks how the batch runs: SQL Server batches report what they
	// produce (see runMSSQL); the others are classified by their first word.
	Driver Driver
}

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
// session connection. A query returns as soon as its columns are known;
// its rows keep arriving in Result.Rows, and the session stays busy until
// fetching ends.
func Run(ctx context.Context, conn *sql.Conn, query string, opts RunOptions) (*Result, error) {
	if opts.MemoryLimit <= 0 {
		opts.MemoryLimit = DefaultMemoryLimit
	}
	if opts.Cancel == nil {
		opts.Cancel = func() {}
	}
	if opts.CheckReadOnly {
		if err := CheckReadOnly(query); err != nil {
			opts.Cancel()
			return nil, err
		}
	}

	start := time.Now()
	if opts.Driver == MySQL {
		res, err := runMySQL(ctx, conn, query, opts)
		if res != nil {
			res.Duration = time.Since(start)
		}
		if res == nil || res.Rows == nil {
			opts.Cancel()
		}
		return res, err
	}
	if opts.Driver == SQLServer {
		res, err := runMSSQL(ctx, conn, query, opts)
		if res != nil {
			res.Duration = time.Since(start)
		}
		if res == nil || res.Rows == nil {
			opts.Cancel()
		}
		return res, err
	}
	res := &Result{}
	var err error
	if returnsRows(query) {
		res.Rows, err = startQuery(ctx, conn, query, opts)
	} else {
		var r sql.Result
		if r, err = conn.ExecContext(ctx, query); err == nil {
			if n, aerr := r.RowsAffected(); aerr == nil {
				res.RowsAffected, res.HasAffected = n, true
			}
		}
	}
	res.Duration = time.Since(start)
	if res.Rows == nil {
		opts.Cancel() // nothing left running
	}
	return res, err
}

// startQuery runs a query and starts fetching its first result set with
// columns; batches may start with row counts from SET or DML statements.
func startQuery(ctx context.Context, conn *sql.Conn, query string, opts RunOptions) (*RowSet, error) {
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
			go set.fetch(ctx, rows, opts.MemoryLimit, opts.Cancel)
			return set, nil
		}
		if !rows.NextResultSet() {
			err := rows.Err()
			rows.Close()
			return nil, err
		}
	}
}

// newRowSet makes the row set of a result with these columns.
func newRowSet(types []*sql.ColumnType, opts RunOptions) *RowSet {
	set := &RowSet{
		columns: make([]Column, len(types)),
		numeric: make([]bool, len(types)),
		seen:    make([]bool, len(types)),
		updated: make(chan struct{}, 1),
		cancel:  opts.Cancel,
	}
	for i, t := range types {
		set.columns[i] = Column{Name: t.Name(), Type: t.DatabaseTypeName()}
	}
	return set
}

func numericType(name string) bool {
	// MySQL names unsigned columns "UNSIGNED INT" and the like.
	switch strings.TrimPrefix(strings.ToUpper(name), "UNSIGNED ") {
	case "INT", "INT2", "INT4", "INT8", "INTEGER", "BIGINT", "SMALLINT", "TINYINT", "MEDIUMINT",
		"NUMERIC", "DECIMAL", "FLOAT", "FLOAT4", "FLOAT8", "REAL", "DOUBLE",
		"MONEY", "SMALLMONEY", "OID", "BIT":
		return true
	}
	return false
}

// hasControl reports bytes that are no text: control characters other than
// tab and line breaks, as in binary values that happen to be valid UTF-8.
func hasControl(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' || c == 0x7f {
			return true
		}
	}
	return false
}

func formatValue(v any, typ string) Value {
	switch v := v.(type) {
	case nil:
		return Value{Text: "NULL", Null: true}
	case []byte:
		// MySQL sends BIT values as bytes, most significant first.
		if strings.EqualFold(typ, "BIT") && len(v) <= 8 {
			var n uint64
			for _, b := range v {
				n = n<<8 | uint64(b)
			}
			return Value{Text: strconv.FormatUint(n, 10)}
		}
		if strings.EqualFold(typ, "UNIQUEIDENTIFIER") && len(v) == 16 {
			var u mssql.UniqueIdentifier
			if u.Scan(v) == nil {
				return Value{Text: u.String()}
			}
		}
		if utf8.Valid(v) && !hasControl(v) {
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
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		e := Error{Message: my.Message, Code: strconv.Itoa(int(my.Number))}
		if my.SQLState != [5]byte{} {
			e.Detail = "SQLSTATE " + string(my.SQLState[:])
		}
		// "… near 'FORM t' at line 2": the line, for the caret.
		if m := mysqlLine.FindStringSubmatch(my.Message); m != nil {
			e.Line, _ = strconv.Atoi(m[1])
		}
		return e
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
