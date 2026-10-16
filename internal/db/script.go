package db

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Scripts are ready-made SQL for an object in the explorer: the first rows
// of a table, an insert to fill in, a call with its parameters. They are
// only written, for a query tab; the user runs them.
//
// Values to fill in are NULL, so a script run as it is changes nothing:
// "WHERE id = NULL" matches no row.

// Script is one ready-made statement.
type Script struct {
	Title  string // as the menu lists it: "Select first 100 rows"
	Name   string // for the tab: "select"
	Writes bool   // changes data or schema when run
	SQL    string
}

// HasScripts reports whether the explorer offers scripts for the object.
func HasScripts(d Driver, o Object) bool {
	switch o.Kind {
	case KindTable, KindView, KindMatView, KindColumn, KindForeignKey, KindFunction, KindProcedure, KindSequence:
		return o.Kind != KindColumn || o.Table != ""
	case KindIndex:
		return !o.Primary && (d != SQLServer || o.Table != "")
	}
	return false
}

// Scripts writes the scripts for an object; rows is the N of "first N
// rows". Tables and keys are read from the catalog for their columns.
func (p *Pool) Scripts(ctx context.Context, o Object, rows int) ([]Script, error) {
	d := p.cfg.Driver
	q := quoter{d}
	switch o.Kind {
	case KindTable, KindView, KindMatView:
		t, err := p.Describe(ctx, o.Database, o.Schema, o.Name)
		if err != nil {
			return nil, err
		}
		if o.Kind == KindTable {
			return tableScripts(q, o, t, rows), nil
		}
		return viewScripts(q, o, t, rows), nil
	case KindColumn:
		return columnScripts(q, o, rows), nil
	case KindForeignKey:
		t, err := p.Describe(ctx, o.Database, o.Schema, o.Table)
		if err != nil {
			return nil, err
		}
		for _, fk := range t.ForeignKeys {
			if fk.Label() == o.Name {
				return foreignKeyScripts(q, o.Schema, fk, rows), nil
			}
		}
		return nil, fmt.Errorf("the foreign key %s is gone: refresh the table", o.Name)
	case KindFunction, KindProcedure:
		r, err := p.Routine(ctx, o)
		if err != nil {
			return nil, err
		}
		return callScripts(q, o, r), nil
	case KindSequence:
		name := q.qualified(o.Schema, o.Name)
		return []Script{
			{Title: "Current value", Name: "value", SQL: "SELECT last_value, is_called\nFROM " + name + ";\n"},
			{Title: "Restart", Name: "restart", Writes: true, SQL: "ALTER SEQUENCE " + name + " RESTART WITH 1;\n"},
		}, nil
	case KindIndex:
		drop := "DROP INDEX " + q.qualified(o.Schema, o.Name)
		switch d {
		case SQLServer:
			drop = "DROP INDEX " + q.ident(o.Name) + " ON " + q.qualified(o.Schema, o.Table)
		case SQLite:
			drop = "DROP INDEX " + q.ident(o.Name)
		}
		return []Script{dropScript("index", drop)}, nil
	}
	return nil, fmt.Errorf("no scripts for this kind of object")
}

func tableScripts(q quoter, o Object, t *Table, rows int) []Script {
	name := q.qualified(o.Schema, t.Name)
	var all, insert, set, key []TableColumn
	for _, c := range t.Columns {
		if !c.Implicit {
			all = append(all, c)
		}
		switch {
		case c.PrimaryKey:
			key = append(key, c)
		case !c.Auto && !c.Implicit:
			set = append(set, c)
		}
		if !c.Auto && !c.Implicit {
			insert = append(insert, c)
		}
	}
	// Without a primary key, SQLite's rowid still names a row.
	if len(key) == 0 {
		for _, c := range t.Columns {
			if c.Implicit {
				key = append(key, c)
			}
		}
	}

	scripts := []Script{
		{Title: fmt.Sprintf("Select first %d rows", rows), Name: "select", SQL: q.selectRows(names(all), name, rows)},
		{Title: "Count rows", Name: "count", SQL: "SELECT COUNT(*)\nFROM " + name + ";\n"},
		{Title: "Insert a row", Name: "insert", Writes: true, SQL: q.insert(name, insert)},
		{Title: "Update a row by its key", Name: "update", Writes: true, SQL: q.update(name, set, key)},
		{Title: "Delete a row by its key", Name: "delete", Writes: true, SQL: "DELETE FROM " + name + "\n" + q.where(key)},
		{Title: "Add a column", Name: "add-column", Writes: true, SQL: q.addColumn(name)},
		dropScript("table", "DROP TABLE "+name),
	}
	return scripts
}

func viewScripts(q quoter, o Object, t *Table, rows int) []Script {
	name := q.qualified(o.Schema, t.Name)
	kind := "VIEW"
	if o.Kind == KindMatView {
		kind = "MATERIALIZED VIEW"
	}
	return []Script{
		{Title: fmt.Sprintf("Select first %d rows", rows), Name: "select", SQL: q.selectRows(names(t.Columns), name, rows)},
		{Title: "Count rows", Name: "count", SQL: "SELECT COUNT(*)\nFROM " + name + ";\n"},
		dropScript(strings.ToLower(kind), "DROP "+kind+" "+name),
	}
}

func columnScripts(q quoter, o Object, rows int) []Script {
	col, table := q.ident(o.Name), q.qualified(o.Schema, o.Table)
	sql := "SELECT " + col + ", COUNT(*) AS occurrences\nFROM " + table + "\nGROUP BY " + col + "\nORDER BY occurrences DESC"
	if q.d == SQLServer {
		sql = "SELECT TOP (" + strconv.Itoa(rows) + ") " + col + ", COUNT(*) AS occurrences\nFROM " + table +
			"\nGROUP BY " + col + "\nORDER BY occurrences DESC;\n"
	} else {
		sql += "\nLIMIT " + strconv.Itoa(rows) + ";\n"
	}
	return []Script{{Title: fmt.Sprintf("Most common values (top %d)", rows), Name: "values", SQL: sql}}
}

func foreignKeyScripts(q quoter, schema string, fk ForeignKey, rows int) []Script {
	refSchema := fk.RefSchema
	if refSchema == "" {
		refSchema = schema
	}
	child, parent := q.qualified(schema, fk.Table), q.qualified(refSchema, fk.RefTable)
	var on []string
	for i := range fk.Columns {
		on = append(on, "p."+q.ident(fk.RefColumns[i])+" = c."+q.ident(fk.Columns[i]))
	}
	join := "FROM " + child + " AS c\n%sJOIN " + parent + " AS p ON " + strings.Join(on, " AND ")
	orphans := []string{"p." + q.ident(fk.RefColumns[0]) + " IS NULL"}
	for _, c := range fk.Columns {
		orphans = append(orphans, "c."+q.ident(c)+" IS NOT NULL")
	}
	return []Script{
		{Title: "Join the two tables", Name: "join",
			SQL: q.limited("SELECT c.*, p.*\n"+fmt.Sprintf(join, ""), rows)},
		{Title: "Rows whose referenced row is missing", Name: "orphans",
			SQL: q.limited("SELECT c.*\n"+fmt.Sprintf(join, "LEFT ")+"\nWHERE "+strings.Join(orphans, "\n  AND "), rows)},
	}
}

func callScripts(q quoter, o Object, r *Routine) []Script {
	name := q.qualified(o.Schema, o.Name)
	var sql string
	switch {
	case q.d == SQLServer && r.Kind == "procedure":
		sql = mssqlExec(q, name, r.Params)
	case q.d == SQLServer && r.Kind == "scalar function":
		sql = "SELECT " + name + q.args(r.Params) + ";\n"
	case r.Kind == "procedure":
		sql = "CALL " + name + q.args(r.Params) + ";\n"
	default:
		sql = "SELECT *\nFROM " + name + q.args(r.Params) + ";\n"
	}
	return []Script{{Title: "Call with its parameters", Name: "call", Writes: r.Kind == "procedure", SQL: sql}}
}

// mssqlExec calls a procedure by parameter name, collecting its OUTPUT
// parameters in variables.
func mssqlExec(q quoter, name string, params []Param) string {
	if len(params) == 0 {
		return "EXEC " + name + ";\n"
	}
	var b strings.Builder
	var outputs []Param
	for _, p := range params {
		if p.Output {
			outputs = append(outputs, p)
			fmt.Fprintf(&b, "DECLARE %s %s;\n", p.Name, p.Type)
		}
	}
	b.WriteString("EXEC ")
	b.WriteString(name)
	for i, p := range params {
		value := "NULL"
		if p.Output {
			value = p.Name + " OUTPUT"
		}
		sep := ","
		if i == len(params)-1 {
			sep = ";"
		}
		fmt.Fprintf(&b, "\n    %s = %s%s -- %s", p.Name, value, sep, p.Type)
	}
	b.WriteString("\n")
	if len(outputs) > 0 {
		var cols []string
		for _, p := range outputs {
			cols = append(cols, p.Name+" AS "+q.ident(strings.TrimPrefix(p.Name, "@")))
		}
		fmt.Fprintf(&b, "SELECT %s;\n", strings.Join(cols, ", "))
	}
	return b.String()
}

// dropScript writes a DROP commented out, so that running the tab as it is
// drops nothing.
func dropScript(what, statement string) Script {
	return Script{
		Title: "Drop the " + what + " (commented out)", Name: "drop", Writes: true,
		SQL: "-- Remove the -- below to drop the " + what + ". This cannot be undone.\n-- " + statement + ";\n",
	}
}

func names(cols []TableColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// quoter writes names and statements in a driver's dialect.
type quoter struct{ d Driver }

var bareName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ident quotes a name only when it needs it: not a plain name, a reserved
// word, or, on PostgreSQL, a name with capitals (which unquoted names lose).
func (q quoter) ident(name string) string {
	plain := bareName.MatchString(name) && !reserved[strings.ToUpper(name)]
	if q.d == Postgres && strings.ToLower(name) != name {
		plain = false
	}
	switch {
	case plain:
		return name
	case q.d == SQLServer:
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// qualified names an object with its schema; SQLite has none, and SQL
// Server's default is dbo.
func (q quoter) qualified(schema, name string) string {
	switch {
	case q.d == SQLite:
		return q.ident(name)
	case schema == "" && q.d == SQLServer:
		schema = "dbo"
	case schema == "":
		return q.ident(name)
	}
	return q.ident(schema) + "." + q.ident(name)
}

// selectRows selects columns of the first rows.
func (q quoter) selectRows(cols []string, from string, rows int) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = q.ident(c)
	}
	list := "*"
	if len(quoted) > 0 {
		list = wrapList(quoted, 7, 72)
	}
	return q.limited("SELECT "+list+"\nFROM "+from, rows)
}

// limited ends a query with LIMIT, or puts TOP in its SELECT.
func (q quoter) limited(query string, rows int) string {
	n := strconv.Itoa(rows)
	if q.d == SQLServer {
		return strings.Replace(query, "SELECT ", "SELECT TOP ("+n+") ", 1) + ";\n"
	}
	return query + "\nLIMIT " + n + ";\n"
}

func (q quoter) insert(table string, cols []TableColumn) string {
	if len(cols) == 0 {
		return "INSERT INTO " + table + " DEFAULT VALUES;\n"
	}
	quoted := make([]string, len(cols))
	values := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = q.ident(c.Name)
		values[i] = "NULL"
	}
	return "INSERT INTO " + table + " (" + wrapList(quoted, len(table)+14, 72) + ")\nVALUES (\n" +
		commented(values, cols, "    ", ",") + "\n);\n"
}

func (q quoter) update(table string, set, key []TableColumn) string {
	if len(set) == 0 {
		return "-- " + table + " has no column to set but its key.\n"
	}
	lines := make([]string, len(set))
	for i, c := range set {
		lines[i] = q.ident(c.Name) + " = NULL"
	}
	return "UPDATE " + table + "\nSET " + strings.TrimPrefix(commented(lines, set, "    ", ","), "    ") + "\n" + q.where(key)
}

// where picks one row by its key, or says there is none.
func (q quoter) where(key []TableColumn) string {
	if len(key) == 0 {
		return "WHERE 1 = 0; -- no primary key: say which rows\n"
	}
	conds := make([]string, len(key))
	for i, c := range key {
		conds[i] = q.ident(c.Name) + " = NULL"
	}
	return "WHERE " + strings.Join(conds, "\n  AND ") + "; -- the row's key\n"
}

func (q quoter) addColumn(table string) string {
	switch q.d {
	case SQLServer:
		return "ALTER TABLE " + table + "\nADD new_column nvarchar(100) NULL;\n"
	case SQLite:
		return "ALTER TABLE " + table + "\nADD COLUMN new_column TEXT;\n"
	}
	return "ALTER TABLE " + table + "\nADD COLUMN new_column text;\n"
}

// args writes a parenthesised argument list, one NULL per parameter, each
// commented with its name and type.
func (q quoter) args(params []Param) string {
	if len(params) == 0 {
		return "()"
	}
	values := make([]string, len(params))
	notes := make([]string, len(params))
	for i, p := range params {
		values[i] = "NULL"
		notes[i] = strings.TrimSpace(p.Name + " " + p.Type)
		if p.Output {
			notes[i] += ", output"
		}
	}
	var b strings.Builder
	b.WriteString("(\n")
	for i := range values {
		sep := ","
		if i == len(values)-1 {
			sep = " "
		}
		fmt.Fprintf(&b, "    %s%s -- %s\n", values[i], sep, notes[i])
	}
	b.WriteString(")")
	return b.String()
}

// commented writes one item per line, each followed by a comment saying
// what its column is.
func commented(items []string, cols []TableColumn, indent, sep string) string {
	width := 0
	for _, it := range items {
		width = max(width, len(it)+len(sep))
	}
	lines := make([]string, len(items))
	for i, it := range items {
		s := sep
		if i == len(items)-1 {
			s = ""
		}
		note := cols[i].Name + " " + cols[i].Type
		if !cols[i].Nullable {
			note += ", not null"
		}
		if cols[i].Default != "" {
			note += ", default " + cols[i].Default
		}
		lines[i] = indent + it + s + strings.Repeat(" ", width-len(it)-len(s)) + " -- " + note
	}
	return strings.Join(lines, "\n")
}

// wrapList joins items with commas, starting a new line, indented, before
// one that would pass width.
func wrapList(items []string, indent, width int) string {
	var b strings.Builder
	col := indent
	for i, it := range items {
		if i > 0 {
			b.WriteString(",")
			col++
			if col+1+len(it) > width {
				b.WriteString("\n" + strings.Repeat(" ", indent))
				col = indent
			} else {
				b.WriteString(" ")
				col++
			}
		}
		b.WriteString(it)
		col += len(it)
	}
	return b.String()
}

// reserved words are quoted when used as names. The list covers the words
// the three dialects reserve that are likely as table or column names;
// quoting one more than needed is harmless.
var reserved = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`
		ADD ALL ALTER AND ANY AS ASC AUTHORIZATION BACKUP BEGIN BETWEEN BREAK BY CASCADE CASE CAST CHECK
		CLOSE COLUMN COMMIT CONSTRAINT CONTINUE CREATE CROSS CURRENT CURRENT_DATE CURRENT_TIME
		CURRENT_TIMESTAMP CURRENT_USER CURSOR DATABASE DEFAULT DELETE DESC DISTINCT DO DROP ELSE END
		ESCAPE EXCEPT EXEC EXECUTE EXISTS FALSE FETCH FILE FOR FOREIGN FROM FULL FUNCTION GRANT GROUP
		HAVING IDENTITY IF IN INDEX INNER INSERT INTERSECT INTO IS JOIN KEY LEFT LIKE LIMIT NATURAL NOT
		NULL OF OFFSET ON OPEN OR ORDER OUTER PERCENT PLAN PRIMARY PROCEDURE PUBLIC REFERENCES RETURN
		RETURNING REVOKE RIGHT ROLLBACK ROWCOUNT RULE SCHEMA SELECT SESSION_USER SET SOME TABLE THEN TO
		TOP TRAN TRANSACTION TRIGGER TRUE UNION UNIQUE UPDATE USER USING VALUES VIEW WHEN WHERE WHILE
		WINDOW WITH`) {
		m[w] = true
	}
	return m
}()
