package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/go-sql-driver/mysql"
)

// Changing a procedure, function, view or trigger: its definition opens in
// a tab ready to run, and running the whole tab applies it, the way each
// engine allows.
//
//	PostgreSQL   CREATE OR REPLACE, as the server writes the definition
//	SQL Server   the module's SET options, GO, then ALTER
//	MariaDB      CREATE OR REPLACE
//	MySQL        no way to change a body in place: the new version is tried
//	             under a temporary name first, so an error leaves the old one,
//	             then dropped and created, in the routine's own sql_mode
//	SQLite       dropped and created inside a savepoint
//
// Before applying, the definition on the server is compared with the one
// the tab opened: someone else's change is not overwritten unasked.

// Change is what a change tab changes, and the definition it started from.
type Change struct {
	Driver   Driver `json:"driver"`
	Kind     Kind   `json:"kind"`
	Database string `json:"database,omitempty"`
	Schema   string `json:"schema,omitempty"`
	Name     string `json:"name"`
	Args     string `json:"args,omitempty"` // PostgreSQL: the overload's identity arguments
	Original string `json:"original"`       // the definition when the tab opened
}

// What names the object for messages: "procedure settle".
func (c *Change) What() string { return KindName(c.Kind) + " " + c.Name }

// ChangedError says the object changed, or went, since the tab opened it.
type ChangedError struct {
	What string
	Gone bool
}

func (e *ChangedError) Error() string {
	if e.Gone {
		return "the " + e.What + " was dropped since you opened it"
	}
	return "the " + e.What + " was changed by someone else since you opened it"
}

// querier is a database handle or a session.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Changeable reports whether the explorer offers to change the object.
func Changeable(d Driver, o Object) bool {
	switch o.Kind {
	case KindProcedure, KindFunction:
		return d != SQLite
	case KindView:
		return true
	case KindTrigger:
		return d == SQLite || d == MySQL // the others' triggers are not in the tree
	}
	return false
}

func changeOf(d Driver, o Object) *Change {
	c := &Change{Driver: d, Kind: o.Kind, Database: o.Database, Schema: o.Schema, Name: o.Name}
	if d == Postgres && (o.Kind == KindFunction || o.Kind == KindProcedure) {
		c.Args = strings.TrimSuffix(strings.TrimPrefix(o.Detail, "("), ")")
	}
	return c
}

// definition is the object's definition as its engine keeps it, and for
// SQL Server the module's SET options, for MySQL its sql_mode.
type definition struct {
	text      string
	ansiNulls bool
	quotedIDs bool
	sqlMode   string
	maria     bool
}

// readDefinition reads the definition; ErrNotFound when the object is gone.
func readDefinition(ctx context.Context, q querier, c *Change) (definition, error) {
	var def definition
	var text sql.NullString
	var err error
	switch c.Driver {
	case Postgres:
		if c.Kind == KindView {
			err = q.QueryRowContext(ctx, `
				SELECT pg_get_viewdef(c.oid, true) FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'v'`, c.Schema, c.Name).Scan(&text)
		} else {
			err = q.QueryRowContext(ctx, `
				SELECT pg_get_functiondef(p.oid) FROM pg_proc p
				JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname = $1 AND p.proname = $2 AND pg_get_function_identity_arguments(p.oid) = $3`,
				c.Schema, c.Name, c.Args).Scan(&text)
		}
	case SQLServer:
		// Temporary procedures live in tempdb, as their catalog does.
		catalog, id := "sys", "OBJECT_ID(QUOTENAME(@p1) + '.' + QUOTENAME(@p2))"
		args := []any{c.Schema, c.Name}
		if c.Schema == "" {
			args[0] = "dbo"
		}
		if strings.HasPrefix(c.Name, "#") {
			catalog, id, args = "tempdb.sys", "OBJECT_ID(N'tempdb..' + @p1)", []any{c.Name}
		}
		err = q.QueryRowContext(ctx, `
			SELECT m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier
			FROM `+catalog+`.sql_modules m WHERE m.object_id = `+id, args...).Scan(&text, &def.ansiNulls, &def.quotedIDs)
		if err == nil && !text.Valid {
			return def, fmt.Errorf("the %s is encrypted: its definition cannot be read", c.What())
		}
	case MySQL:
		def, err = mysqlDefinition(ctx, q, c)
		if err == nil {
			return def, nil
		}
	case SQLite:
		typ := "view"
		if c.Kind == KindTrigger {
			typ = "trigger"
		}
		err = q.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`, typ, c.Name).Scan(&text)
	default:
		return def, fmt.Errorf("%s objects cannot be changed", c.Driver.Label())
	}
	if errors.Is(err, sql.ErrNoRows) || err == nil && !text.Valid {
		return def, ErrNotFound
	}
	def.text = text.String
	return def, err
}

// mysqlDefinition reads SHOW CREATE: the statement, and for routines and
// triggers the sql_mode they were created in.
func mysqlDefinition(ctx context.Context, q querier, c *Change) (definition, error) {
	var def definition
	var version string
	if err := q.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return def, err
	}
	def.maria = strings.Contains(version, "MariaDB")
	kind := strings.ToUpper(KindName(c.Kind))
	name := quoter{MySQL}.qualified(c.Schema, c.Name)
	conn, ok := q.(interface {
		QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	})
	if !ok {
		return def, errors.New("mysql: cannot read the definition")
	}
	rows, err := conn.QueryContext(ctx, "SHOW CREATE "+kind+" "+name)
	if err != nil {
		// 1305 no such routine, 1146 no such view, 1360 no such trigger.
		var my *mysql.MySQLError
		if errors.As(err, &my) && (my.Number == 1305 || my.Number == 1146 || my.Number == 1360) {
			return def, ErrNotFound
		}
		return def, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return def, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return def, err
		}
		return def, ErrNotFound
	}
	values := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return def, err
	}
	for i, col := range cols {
		switch strings.ToLower(col) {
		case "create procedure", "create function", "create view", "sql original statement":
			if !values[i].Valid {
				return def, fmt.Errorf("the %s's definition is not readable: only its definer, or a user with the SHOW_ROUTINE privilege, can read it", c.What())
			}
			def.text = values[i].String
		case "sql_mode":
			def.sqlMode = values[i].String
		}
	}
	if def.text == "" {
		return def, ErrNotFound
	}
	return def, rows.Err()
}

// changeScript is the "change" script of an object: its definition, ready
// to run, with what running it does.
func (p *Pool) changeScript(ctx context.Context, o Object) (Script, error) {
	c := changeOf(p.cfg.Driver, o)
	conn, err := p.DB(ctx, o.Database)
	if err != nil {
		return Script{}, err
	}
	def, err := readDefinition(ctx, conn, c)
	if err != nil {
		return Script{}, err
	}
	c.Original = def.text
	what := KindName(c.Kind)
	s := Script{Title: "Change the " + what, Name: "change", Writes: true, Change: c}

	switch c.Driver {
	case Postgres:
		if c.Kind == KindView {
			s.SQL = "-- Running this replaces the view. Its columns can change only at the end:\n" +
				"-- new ones after the old, none dropped or renamed.\n" +
				"CREATE OR REPLACE VIEW " + quoter{Postgres}.qualified(c.Schema, c.Name) + " AS\n" + def.text + "\n"
		} else {
			s.SQL = "-- Running this replaces the " + what + ". Changing its parameters' types creates\n" +
				"-- another " + what + " beside this one instead: drop the old one then.\n" +
				strings.TrimRight(def.text, "\n") + ";\n"
		}
	case SQLServer:
		alter, err := mssqlAlter(def.text)
		if err != nil {
			return Script{}, err
		}
		s.SQL = "-- The " + what + "'s own settings, which ALTER takes from the session.\n" +
			"SET ANSI_NULLS " + onOff(def.ansiNulls) + ";\nSET QUOTED_IDENTIFIER " + onOff(def.quotedIDs) + ";\nGO\n" +
			strings.TrimRight(alter, "\n") + "\n"
	case MySQL:
		text := strings.TrimRight(def.text, "\n;") + ";\n"
		switch {
		case def.maria || c.Kind == KindView:
			s.SQL = "-- Running this replaces the " + what + ".\n" + orReplace(text)
		default:
			s.SQL = "-- MySQL cannot change a " + what + " in place. Running the whole tab tries the new\n" +
				"-- version under a temporary name first, so an error leaves this one as it is,\n" +
				"-- then drops the " + what + " and creates it again"
			if c.Kind != KindTrigger {
				s.SQL += ": privileges granted on it\n-- are lost, grant them again"
			}
			s.SQL += ".\n" + text
		}
	case SQLite:
		s.SQL = "-- SQLite cannot change a " + what + " in place. Running the whole tab drops it and\n" +
			"-- creates it again inside a savepoint: an error leaves it as it was.\n" +
			strings.TrimRight(def.text, "\n;") + ";\n"
	}
	return s, nil
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// mssqlAlter turns a module's CREATE into ALTER, past any comments before
// it; CREATE OR ALTER stays as it is.
func mssqlAlter(def string) (string, error) {
	i := skipComments(def, 0)
	if !hasWordAt(def, i, "CREATE") {
		return "", errors.New("the definition does not start with CREATE")
	}
	if j := skipComments(def, i+len("CREATE")); hasWordAt(def, j, "OR") {
		return def, nil // CREATE OR ALTER
	}
	return def[:i] + "ALTER" + def[i+len("CREATE"):], nil
}

// orReplace makes CREATE … into CREATE OR REPLACE …, unless it is already.
func orReplace(def string) string {
	i := skipComments(def, 0)
	if !hasWordAt(def, i, "CREATE") {
		return def
	}
	if j := skipComments(def, i+len("CREATE")); hasWordAt(def, j, "OR") {
		return def
	}
	return def[:i] + "CREATE OR REPLACE" + def[i+len("CREATE"):]
}

// skipComments skips blanks and comments (--, #, /* */) from i.
func skipComments(s string, i int) int {
	for i < len(s) {
		switch {
		case unicode.IsSpace(rune(s[i])):
			i++
		case strings.HasPrefix(s[i:], "--") || s[i] == '#':
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				return len(s)
			}
		case strings.HasPrefix(s[i:], "/*"):
			depth := 0
			for i < len(s) {
				if strings.HasPrefix(s[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(s[i:], "*/") {
					depth--
					i += 2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
		default:
			return i
		}
	}
	return i
}

// hasWordAt reports the keyword word at i, ignoring case, as a whole word.
func hasWordAt(s string, i int, word string) bool {
	if i+len(word) > len(s) || !strings.EqualFold(s[i:i+len(word)], word) {
		return false
	}
	return i+len(word) == len(s) || !isWordByte(s[i+len(word)])
}

func isWordByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// ApplyChange applies a change tab: it checks that the object is still as
// the tab opened it (unless force), then applies text the engine's way.
// Once the run is over, Definition reads what the object became.
func ApplyChange(ctx context.Context, s *sql.Conn, c Change, text string, opts RunOptions, force bool) (*Result, error) {
	if opts.Cancel == nil {
		opts.Cancel = func() {}
	}
	def, err := readDefinition(ctx, s, &c)
	gone := errors.Is(err, ErrNotFound)
	if err != nil && !gone {
		opts.Cancel()
		return nil, err
	}
	if !force && (gone || def.text != c.Original) {
		opts.Cancel()
		return nil, &ChangedError{What: c.What(), Gone: gone}
	}
	switch {
	case c.Driver == MySQL && !def.maria && c.Kind != KindView:
		defer opts.Cancel()
		return &Result{}, mysqlReplace(ctx, s, c, def, text)
	case c.Driver == SQLite:
		defer opts.Cancel()
		return &Result{}, sqliteReplace(ctx, s, c, text)
	}
	opts.Driver = c.Driver
	return Run(ctx, s, text, opts)
}

// Definition is the object's definition on the server now, as a change
// tab compares it.
func Definition(ctx context.Context, s *sql.Conn, c Change) (string, error) {
	def, err := readDefinition(ctx, s, &c)
	return def.text, err
}

// mysqlReplace tries the new version of a routine or trigger under a
// temporary name, then drops the old one and creates the new, in the sql_mode
// it was created in. If the new one cannot be created after the drop, the
// old one is put back.
func mysqlReplace(ctx context.Context, s *sql.Conn, c Change, def definition, text string) error {
	stmt := strings.TrimRight(strings.TrimSpace(text), ";")
	start, end, ok := mysqlNameSpan(stmt, strings.ToUpper(KindName(c.Kind)))
	if !ok {
		return fmt.Errorf("rimor cannot find the %s's name in the statement", KindName(c.Kind))
	}
	if schema, last := mysqlSplitName(stmt[start:end]); !strings.EqualFold(last, c.Name) ||
		schema != "" && !strings.EqualFold(schema, c.Schema) {
		return fmt.Errorf("the statement creates %s, not the %s: running the whole tab changes only that", stmt[start:end], c.What())
	}
	q := quoter{MySQL}
	tryName := mysqlTryName(stmt[start:end])
	try := stmt[:start] + tryName + stmt[end:]
	kind := strings.ToUpper(KindName(c.Kind))

	if def.sqlMode != "" {
		var old string
		if err := s.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&old); err != nil {
			return err
		}
		if _, err := s.ExecContext(ctx, "SET SESSION sql_mode = ?", def.sqlMode); err != nil {
			return err
		}
		defer s.ExecContext(context.WithoutCancel(ctx), "SET SESSION sql_mode = ?", old)
	}
	if _, err := s.ExecContext(ctx, try); err != nil {
		return fmt.Errorf("the new version has an error, so the %s was left as it was: %w", c.What(), err)
	}
	if _, err := s.ExecContext(ctx, "DROP "+kind+" "+tryName); err != nil {
		return err
	}
	name := q.qualified(c.Schema, c.Name)
	if _, err := s.ExecContext(ctx, "DROP "+kind+" IF EXISTS "+name); err != nil {
		return err
	}
	if _, err := s.ExecContext(ctx, stmt); err != nil {
		if c.Original != "" {
			if _, rerr := s.ExecContext(ctx, c.Original); rerr == nil {
				return fmt.Errorf("the new version could not be created, so the old one was put back: %w", err)
			}
		}
		return fmt.Errorf("the %s was dropped but could not be created again: %w", c.What(), err)
	}
	return nil
}

// sqliteReplace drops a view or trigger and creates it again from text,
// inside a savepoint, which works within a transaction too.
func sqliteReplace(ctx context.Context, s *sql.Conn, c Change, text string) error {
	if _, err := s.ExecContext(ctx, "SAVEPOINT rimor_change"); err != nil {
		return err
	}
	undo := func(err error) error {
		s.ExecContext(ctx, "ROLLBACK TO rimor_change")
		s.ExecContext(ctx, "RELEASE rimor_change")
		return err
	}
	kind := KindName(c.Kind)
	for _, stmt := range []string{"DROP " + strings.ToUpper(kind) + " IF EXISTS " + quoter{SQLite}.ident(c.Name), text} {
		if _, err := s.ExecContext(ctx, stmt); err != nil {
			return undo(err)
		}
	}
	// The text could be anything: it must have made the object again.
	var n int
	if err := s.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = ? AND name = ? COLLATE NOCASE", kind, c.Name).Scan(&n); err != nil {
		return undo(err)
	}
	if n == 0 {
		return undo(fmt.Errorf("the tab does not create the %s, so it was left as it was: running the whole tab changes only that", c.What()))
	}
	_, err := s.ExecContext(ctx, "RELEASE rimor_change")
	return err
}

// mysqlNameSpan finds the name after the statement's PROCEDURE, FUNCTION or
// TRIGGER keyword (kind): `db`.`name`, db.name, `name` or name. Comments,
// strings and quoted names before it are skipped, so a DEFINER or a
// comment cannot be taken for it.
func mysqlNameSpan(stmt, kind string) (start, end int, ok bool) {
	i := 0
	for i < len(stmt) {
		i = skipComments(stmt, i)
		if i >= len(stmt) {
			return 0, 0, false
		}
		switch c := stmt[i]; {
		case c == '`' || c == '\'' || c == '"':
			i = skipQuoted(stmt, i)
		case isWordByte(c):
			j := i
			for j < len(stmt) && isWordByte(stmt[j]) {
				j++
			}
			if strings.EqualFold(stmt[i:j], kind) {
				start = skipComments(stmt, j)
				end = mysqlIdent(stmt, start)
				if end == start {
					return 0, 0, false
				}
				if dot := skipComments(stmt, end); dot < len(stmt) && stmt[dot] == '.' {
					if e := mysqlIdent(stmt, skipComments(stmt, dot+1)); e > dot+1 {
						end = e
					}
				}
				return start, end, true
			}
			i = j
		default:
			i++
		}
	}
	return 0, 0, false
}

// mysqlIdent is the end of the identifier at i, quoted or not.
func mysqlIdent(s string, i int) int {
	if i < len(s) && s[i] == '`' {
		return skipQuoted(s, i)
	}
	j := i
	for j < len(s) && isWordByte(s[j]) {
		j++
	}
	return j
}

// skipQuoted is the end of the quoted text starting at i, its quote
// doubled inside.
func skipQuoted(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\\' && q != '`':
			j++
		case s[j] == q && j+1 < len(s) && s[j+1] == q:
			j++
		case s[j] == q:
			return j + 1
		}
	}
	return len(s)
}

// mysqlSplitName unquotes a name as mysqlNameSpan finds it: its database,
// if given, and the name.
func mysqlSplitName(name string) (schema, last string) {
	unquote := func(p string) string { return strings.ReplaceAll(strings.Trim(p, "`"), "``", "`") }
	if i := strings.LastIndex(name, "."); i > 0 && (strings.HasSuffix(name[:i], "`") || !strings.Contains(name[:i], "`")) {
		return unquote(strings.TrimSpace(name[:i])), unquote(strings.TrimSpace(name[i+1:]))
	}
	return "", unquote(name)
}

// mysqlTryName names the trial version: the name with _rimor_try after it,
// in the same database.
func mysqlTryName(name string) string {
	schema, last := mysqlSplitName(name)
	q := quoter{MySQL}
	if schema != "" {
		return q.qualified(schema, last+"_rimor_try")
	}
	return q.ident(last + "_rimor_try")
}
