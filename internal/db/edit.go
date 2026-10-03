package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/stdlib"
)

// NotEditable explains why a result, or one of its columns, cannot be
// edited. The message is shown to the user as is.
type NotEditable struct{ Reason string }

func (e NotEditable) Error() string { return e.Reason }

func notEditable(format string, args ...any) error {
	return NotEditable{fmt.Sprintf(format, args...)}
}

// ErrRowChanged means the row no longer holds the value it was edited
// from, or no longer exists: someone changed it meanwhile.
var ErrRowChanged = errors.New("the row changed since it was loaded, or was deleted; run the query again to see it now")

// Origin maps a result back to the one table its editable columns come
// from, with that table's key among the result columns.
type Origin struct {
	driver Driver
	table  string // quoted, qualified name for SQL

	// Per result column: the source column's quoted name and SQL type, or
	// "" when the result column does not come from the table (expressions,
	// other tables) or cannot be changed (computed, generated, identity).
	columns []string
	types   []string
	// Readable name of the table and of each source column, for messages
	// and the statement preview.
	Table string
	names []string
	// fixed marks source columns the database computes (generated,
	// computed or identity columns).
	fixed []bool
	// json marks text columns kept to JSON by the schema: an ISJSON check
	// on SQL Server, a JSON declared type or json_valid check on SQLite.
	// They edit like PostgreSQL's json. jsonb marks SQLite's binary JSONB,
	// edited as text through json() and jsonb().
	json, jsonb []bool

	key []int // result columns of the key
}

// Editable reports whether a result column can be edited, or why not.
func (o *Origin) Editable(col int) error {
	for _, k := range o.key {
		if k == col {
			return notEditable("Key columns cannot be edited here; rimor finds the row by them.")
		}
	}
	if o.fixed[col] {
		return notEditable("The database computes this column, so it cannot be edited.")
	}
	if o.columns[col] == "" {
		return notEditable("This column is not a column of %s.", o.Table)
	}
	return nil
}

// FindOrigin works out where a query's result comes from. It does not run
// the query: PostgreSQL and SQLite describe the prepared statement, SQL
// Server analyses it with sp_describe_first_result_set.
func FindOrigin(ctx context.Context, conn *sql.Conn, d Driver, query string, columns int) (*Origin, error) {
	switch d {
	case Postgres:
		return describePostgres(ctx, conn, query, columns)
	case SQLServer:
		return describeSQLServer(ctx, conn, query, columns)
	case SQLite:
		return describeSQLite(ctx, conn, query, columns)
	}
	return nil, notEditable("Editing results is not available for %s.", d.Label())
}

func describePostgres(ctx context.Context, conn *sql.Conn, query string, columns int) (*Origin, error) {
	type field struct {
		table  uint32
		attnum uint16
	}
	var fields []field
	err := conn.Raw(func(dc any) error {
		c, ok := dc.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected driver connection %T", dc)
		}
		sd, err := c.Conn().Prepare(ctx, "", query) // describes; does not run
		if err != nil {
			return err
		}
		for _, f := range sd.Fields {
			fields = append(fields, field{f.TableOID, f.TableAttributeNumber})
		}
		return nil
	})
	if err != nil {
		return nil, notEditable("rimor cannot tell which table this result comes from: %v", firstLine(err.Error()))
	}
	if len(fields) != columns {
		return nil, notEditable("rimor cannot tell which table this result comes from.")
	}

	var table uint32
	for _, f := range fields {
		switch {
		case f.table == 0:
		case table == 0:
			table = f.table
		case f.table != table:
			return nil, notEditable("The result joins several tables; editing needs a result from one table.")
		}
	}
	if table == 0 {
		return nil, notEditable("No column of the result comes straight from a table.")
	}

	o := newOrigin(Postgres, columns)
	var kind string
	if err := conn.QueryRowContext(ctx, `
		SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname), n.nspname || '.' || c.relname, c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = $1`, table,
	).Scan(&o.table, &o.Table, &kind); err != nil {
		return nil, err
	}
	if kind != "r" && kind != "p" {
		return nil, notEditable("%s is not a table.", o.Table)
	}
	if err := singleUse(query, o.Table); err != nil {
		return nil, err
	}

	// The source columns; generated ones are computed by the database.
	type attr struct {
		name, quoted, typ string
		generated         bool
	}
	attrs := map[uint16]attr{}
	rows, err := conn.QueryContext(ctx, `
		SELECT attnum, attname, quote_ident(attname), format_type(atttypid, atttypmod),
		       attgenerated <> '' OR attidentity = 'a'
		FROM pg_attribute
		WHERE attrelid = $1 AND attnum > 0 AND NOT attisdropped`, table)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n int
		var a attr
		if err := rows.Scan(&n, &a.name, &a.quoted, &a.typ, &a.generated); err != nil {
			rows.Close()
			return nil, err
		}
		attrs[uint16(n)] = a
	}
	rows.Close()
	attnumAt := map[uint16]int{} // first result column of each source column
	for i, f := range fields {
		if a, ok := attrs[f.attnum]; ok && f.table == table {
			o.columns[i], o.types[i], o.names[i], o.fixed[i] = a.quoted, a.typ, a.name, a.generated
			if _, seen := attnumAt[f.attnum]; !seen {
				attnumAt[f.attnum] = i
			}
		}
	}

	// The primary key, else a unique index on non-null columns without
	// expressions or predicates; all of its columns must be in the result.
	rows, err = conn.QueryContext(ctx, `
		SELECT i.indkey::text
		FROM pg_index i
		WHERE i.indrelid = $1 AND i.indisunique AND i.indexprs IS NULL AND i.indpred IS NULL
		  AND NOT EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = i.indrelid
		                  AND a.attnum = ANY(i.indkey) AND NOT a.attnotnull)
		ORDER BY i.indisprimary DESC, i.indnatts`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hasKey := false
	for rows.Next() {
		var indkey string
		if err := rows.Scan(&indkey); err != nil {
			return nil, err
		}
		hasKey = true
		var key []int
		for _, part := range strings.Fields(indkey) {
			var n uint16
			fmt.Sscan(part, &n)
			i, ok := attnumAt[n]
			if !ok {
				key = nil
				break
			}
			key = append(key, i)
		}
		if key != nil {
			o.key = key
			return o, nil
		}
	}
	if !hasKey {
		return nil, notEditable("%s has no primary key or unique key, so rimor cannot find a row again.", o.Table)
	}
	return nil, notEditable("Add the key of %s to the SELECT to edit it.", o.Table)
}

func describeSQLServer(ctx context.Context, conn *sql.Conn, query string, columns int) (*Origin, error) {
	rows, err := conn.QueryContext(ctx,
		`EXEC sp_describe_first_result_set @tsql = @p1, @params = NULL, @browse_information_mode = 1`, query)
	if err != nil {
		return nil, notEditable("rimor cannot tell which table this result comes from: %v", firstLine(Describe(err).Message))
	}
	defer rows.Close()

	names, _ := rows.Columns()
	idx := map[string]int{}
	for i, n := range names {
		idx[n] = i
	}
	type col struct {
		hidden, key, computed, identity bool
		db, schema, table, column, typ  string
	}
	var cols []col
	raw := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	str := func(name string) string {
		if v, ok := raw[idx[name]].(string); ok {
			return v
		}
		return ""
	}
	flag := func(name string) bool {
		v, _ := raw[idx[name]].(bool)
		return v
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		cols = append(cols, col{
			hidden: flag("is_hidden"), key: flag("is_part_of_unique_key"),
			computed: flag("is_computed_column"), identity: flag("is_identity_column"),
			db: str("source_database"), schema: str("source_schema"), table: str("source_table"),
			column: str("source_column"), typ: str("system_type_name"),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Visible columns are the result's; hidden ones are keys added by
	// browse mode that the result does not contain.
	var visible []col
	hiddenKey := false
	for _, c := range cols {
		if c.hidden {
			hiddenKey = hiddenKey || c.key
			continue
		}
		visible = append(visible, c)
	}
	if len(visible) != columns {
		return nil, notEditable("rimor cannot tell which table this result comes from.")
	}

	var source string
	for _, c := range visible {
		if c.table == "" {
			continue
		}
		t := c.db + "." + c.schema + "." + c.table
		if source == "" {
			source = t
		} else if t != source {
			return nil, notEditable("The result joins several tables; editing needs a result from one table.")
		}
	}
	if source == "" {
		return nil, notEditable("No column of the result comes straight from a table.")
	}

	o := newOrigin(SQLServer, columns)
	for i, c := range visible {
		if c.table == "" || c.db+"."+c.schema+"."+c.table != source {
			continue
		}
		if o.table == "" {
			o.table = mssqlQuote(c.db) + "." + mssqlQuote(c.schema) + "." + mssqlQuote(c.table)
			o.Table = c.schema + "." + c.table
		}
		if c.key {
			o.key = append(o.key, i)
		}
		o.columns[i], o.types[i], o.names[i] = mssqlQuote(c.column), c.typ, c.column
		o.fixed[i] = c.computed || c.identity
	}
	if err := singleUse(query, o.Table); err != nil {
		return nil, err
	}
	if err := o.findISJSON(ctx, conn); err != nil {
		return nil, err
	}
	if hiddenKey || len(o.key) == 0 {
		if len(o.key) == 0 && !hiddenKey {
			return nil, notEditable("%s has no primary key or unique key, so rimor cannot find a row again.", o.Table)
		}
		return nil, notEditable("Add the key of %s to the SELECT to edit it.", o.Table)
	}
	return o, nil
}

// singleUse refuses queries that name the table more than once, such as
// self-joins: the database reports columns by table, not by alias, so a
// column of one occurrence could be edited through the key of another.
func singleUse(query, table string) error {
	name := table
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if n := countName(query, name); n > 1 {
		return notEditable("%s appears %d times in the query, so rimor cannot tell which row a value belongs to.", table, n)
	}
	return nil
}

// countName counts a name among a query's identifiers, bare or quoted
// ("name", [name], `+"`name`"+`), ignoring case, comments and strings.
func countName(query, name string) int {
	n := 0
	r := []rune(query)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case c == '-' && i+1 < len(r) && r[i+1] == '-':
			for i < len(r) && r[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			i += 2
			for i+1 < len(r) && !(r[i] == '*' && r[i+1] == '/') {
				i++
			}
			i += 2
		case c == '\'':
			i++
			for i < len(r) && !(r[i] == '\'' && (i+1 >= len(r) || r[i+1] != '\'')) {
				if r[i] == '\'' {
					i++
				}
				i++
			}
			i++
		case c == '"' || c == '[' || c == '`':
			end := c
			if c == '[' {
				end = ']'
			}
			start := i + 1
			i++
			for i < len(r) && r[i] != end {
				i++
			}
			if strings.EqualFold(string(r[start:min(i, len(r))]), name) {
				n++
			}
			i++
		case unicode.IsLetter(c) || c == '_':
			start := i
			for i < len(r) && (unicode.IsLetter(r[i]) || unicode.IsDigit(r[i]) || r[i] == '_' || r[i] == '$') {
				i++
			}
			if strings.EqualFold(string(r[start:i]), name) {
				n++
			}
		default:
			i++
		}
	}
	return n
}

func newOrigin(d Driver, columns int) *Origin {
	return &Origin{
		driver:  d,
		columns: make([]string, columns), types: make([]string, columns),
		names: make([]string, columns), fixed: make([]bool, columns),
		json: make([]bool, columns), jsonb: make([]bool, columns),
	}
}

// IsJSON reports whether a text column is kept to JSON by an ISJSON check
// constraint, the usual way to store JSON before SQL Server's json type.
func (o *Origin) IsJSON(col int) bool { return o.json[col] }

// IsJSONB reports SQLite's binary JSON: edited as text, pretty-printed,
// since the binary form keeps no formatting.
func (o *Origin) IsJSONB(col int) bool { return o.jsonb[col] }

// findISJSON marks the columns covered by an enabled ISJSON check. SQL
// Server stores constraint text normalised, as (isjson([doc])=(1)) or,
// with a type argument, isjson([doc],object), so matching "isjson([name]"
// finds both. One catalog query per result.
func (o *Origin) findISJSON(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT DISTINCT c.name
		FROM sys.check_constraints cc
		JOIN sys.columns c ON c.object_id = cc.parent_object_id
		WHERE cc.parent_object_id = OBJECT_ID(@p1) AND cc.is_disabled = 0
		  AND CHARINDEX(LOWER(N'isjson(' + QUOTENAME(c.name)), LOWER(cc.definition)) > 0`, o.table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		for i, n := range o.names {
			if n == name && !o.fixed[i] {
				o.json[i] = true
			}
		}
	}
	return rows.Err()
}

func mssqlQuote(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Cell is one value of a row, located by the row's key.
type Cell struct {
	Origin *Origin
	Col    int
	// Key holds the key values of the row, in the order of Origin.key, as
	// the result shows them.
	Key []string
}

// KeyValues picks a row's key values out of its result cells.
func (o *Origin) KeyValues(row func(col int) Value) ([]string, error) {
	vals := make([]string, len(o.key))
	for i, k := range o.key {
		v := row(k)
		if v.Null {
			return nil, notEditable("The row's key is NULL, so rimor cannot find it again.")
		}
		vals[i] = v.Text
	}
	return vals, nil
}

// Current is a cell's value when editing began.
type Current struct {
	// Text is the value as the database writes it, for the editor; nil
	// for NULL.
	Text *string
	// check is what the update compares the stored value against, to
	// refuse an edit of a row changed meanwhile; nil for NULL.
	check *string
}

// display is the column as text for the editor, the way the database
// writes it: closer to what is stored than the grid's formatting.
func (c Cell) display() string {
	col := c.Origin.columns[c.Col]
	switch c.Origin.driver {
	case Postgres:
		return col + "::text"
	case SQLite:
		if c.Origin.jsonb[c.Col] {
			return "json(" + col + ")" // JSONB is binary; edit it as text
		}
		return "CAST(" + col + " AS TEXT)"
	}
	style := ""
	switch t := strings.ToLower(c.Origin.types[c.Col]); {
	case strings.HasPrefix(t, "date"), strings.HasPrefix(t, "time"), strings.HasPrefix(t, "smalldatetime"):
		style = ", 126" // ISO 8601, full precision
	case strings.HasPrefix(t, "float"), strings.HasPrefix(t, "real"):
		style = ", 3" // all significant digits
	}
	return "CONVERT(nvarchar(max), " + col + style + ")"
}

// check is the column as the update compares it. SQLite values carry
// their own type, so there quote() writes the value as an SQL literal,
// which tells 5 from '5'; elsewhere the display text is exact.
func (c Cell) check() string {
	if c.Origin.driver == SQLite {
		return "quote(" + c.Origin.columns[c.Col] + ")"
	}
	return c.display()
}

// param is the n-th (1-based) statement parameter, cast to a column type.
// SQLite converts by the column's affinity instead.
func (o *Origin) param(n int, typ string) string {
	switch o.driver {
	case Postgres:
		return fmt.Sprintf("CAST(CAST($%d AS text) AS %s)", n, typ)
	case SQLite:
		return fmt.Sprintf("?%d", n)
	}
	return fmt.Sprintf("CAST(@p%d AS %s)", n, typ)
}

// value is the parameter that sets a column.
func (c Cell) value(n int) string {
	if c.Origin.driver == SQLite && c.Origin.jsonb[c.Col] {
		return fmt.Sprintf("jsonb(?%d)", n) // edited as text, stored binary
	}
	return c.Origin.param(n, c.Origin.types[c.Col])
}

func (o *Origin) placeholder(n int) string {
	switch o.driver {
	case Postgres:
		return fmt.Sprintf("$%d", n)
	case SQLite:
		return fmt.Sprintf("?%d", n)
	}
	return fmt.Sprintf("@p%d", n)
}

// where matches the row by its key, with parameters from first on.
func (c Cell) where(first int) (string, []any) {
	var conds []string
	var args []any
	for i, k := range c.Origin.key {
		conds = append(conds, c.Origin.columns[k]+" = "+c.Origin.param(first+i, c.Origin.types[k]))
		args = append(args, c.Key[i])
	}
	return strings.Join(conds, " AND "), args
}

// Current reads the cell's value as the database writes it and checks
// that the key still finds exactly one row.
func (c Cell) Current(ctx context.Context, conn *sql.Conn) (Current, error) {
	where, args := c.where(1)
	rows, err := conn.QueryContext(ctx, "SELECT "+c.display()+", "+c.check()+" FROM "+c.Origin.table+" WHERE "+where, args...)
	if err != nil {
		return Current{}, err
	}
	defer rows.Close()
	var vals []Current
	for rows.Next() {
		var text, check sql.NullString
		if err := rows.Scan(&text, &check); err != nil {
			return Current{}, err
		}
		var cur Current
		if text.Valid {
			cur.Text = &text.String
		}
		if check.Valid && !(c.Origin.driver == SQLite && check.String == "NULL") {
			cur.check = &check.String
		}
		vals = append(vals, cur)
	}
	if err := rows.Err(); err != nil {
		return Current{}, err
	}
	switch len(vals) {
	case 0:
		return Current{}, ErrRowChanged
	case 1:
		return vals[0], nil
	}
	return Current{}, notEditable("The key matches %d rows, so rimor cannot tell which one to change.", len(vals))
}

// Update sets the cell to value (nil for NULL) if it still holds what
// Current read, and returns the new value as the result would show it.
func (c Cell) Update(ctx context.Context, conn *sql.Conn, old Current, value *string) (Value, error) {
	o := c.Origin
	set := "NULL"
	var args []any
	if value != nil {
		set = c.value(1)
		args = append(args, *value)
	}
	where, keyArgs := c.where(len(args) + 1)
	args = append(args, keyArgs...)

	// Only if the value is still the one the edit started from.
	n := len(args) + 1
	if old.check == nil {
		where += " AND " + o.columns[c.Col] + " IS NULL"
	} else {
		where += " AND " + c.check() + " = " + o.placeholder(n)
		args = append(args, *old.check)
	}

	res, err := conn.ExecContext(ctx, "UPDATE "+o.table+" SET "+o.columns[c.Col]+" = "+set+" WHERE "+where, args...)
	if err != nil {
		return Value{}, err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return Value{}, ErrRowChanged
	}
	return c.Reload(ctx, conn)
}

// Reload reads the cell as the result would show it, after an update:
// triggers, defaults and type conversion may have changed what was typed.
func (c Cell) Reload(ctx context.Context, conn *sql.Conn) (Value, error) {
	where, args := c.where(1)
	rows, err := conn.QueryContext(ctx, "SELECT "+c.Origin.columns[c.Col]+" FROM "+c.Origin.table+" WHERE "+where, args...)
	if err != nil {
		return Value{}, err
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return Value{}, err
	}
	if !rows.Next() {
		return Value{}, ErrRowChanged
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		return Value{}, err
	}
	return formatValue(v, types[0].DatabaseTypeName()), rows.Err()
}

// Preview is the update as SQL with the values written in, for the user
// to read; the real statement passes them as parameters.
func (c Cell) Preview(value *string) string {
	o := c.Origin
	lit := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	set := "NULL"
	if value != nil {
		set = lit(*value)
		if o.driver == SQLite && o.jsonb[c.Col] {
			set = "jsonb(" + set + ")"
		}
	}
	var conds []string
	for i, k := range o.key {
		conds = append(conds, o.names[k]+" = "+lit(c.Key[i]))
	}
	return "UPDATE " + o.Table + " SET " + o.names[c.Col] + " = " + set + " WHERE " + strings.Join(conds, " AND ")
}
