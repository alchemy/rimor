package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// What an agent asks about the catalog (see package agent): a table in
// full, names matching a search, and the server's version. Unlike the
// explorer's lazy levels, each answer is complete in one call.

// Table describes a table or view.
type Table struct {
	Kind    string `json:"kind"` // table, view or materialized view
	Schema  string `json:"schema,omitempty"`
	Name    string `json:"name"`
	Comment string `json:"comment,omitempty"`

	Columns []TableColumn `json:"columns"`
	Indexes []TableIndex  `json:"indexes,omitempty"`
	// ForeignKeys are this table's references to others; ReferencedBy are
	// other tables' references to this one.
	ForeignKeys  []ForeignKey `json:"foreign_keys,omitempty"`
	ReferencedBy []ForeignKey `json:"referenced_by,omitempty"`
}

type TableColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	Default    string `json:"default,omitempty"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
	Comment    string `json:"comment,omitempty"`
	// Implicit marks SQLite's rowid, which the table has without declaring.
	Implicit bool `json:"implicit,omitempty"`
}

type TableIndex struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Primary bool     `json:"primary,omitempty"`
}

// ForeignKey is a reference from Columns of Schema.Table to RefColumns of
// RefSchema.RefTable.
type ForeignKey struct {
	Name       string   `json:"name,omitempty"`
	Schema     string   `json:"schema,omitempty"`
	Table      string   `json:"table"`
	Columns    []string `json:"columns"`
	RefSchema  string   `json:"ref_schema,omitempty"`
	RefTable   string   `json:"ref_table"`
	RefColumns []string `json:"ref_columns"`
}

// Label is the foreign key as the explorer shows it under its table:
// "customer_id → customers(id)", schema-qualified when the referenced table
// is in another schema.
func (fk ForeignKey) Label() string {
	ref := fk.RefTable
	if fk.RefSchema != "" && fk.RefSchema != fk.Schema {
		ref = fk.RefSchema + "." + ref
	}
	return strings.Join(fk.Columns, ", ") + " → " + ref + "(" + strings.Join(fk.RefColumns, ", ") + ")"
}

// Match is a name found by Search: an object, or a column of a table or
// view (Column set).
type Match struct {
	Kind   string `json:"kind"` // table, view, materialized view, function, procedure, column
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	Column string `json:"column,omitempty"`
	Type   string `json:"type,omitempty"` // the column's type
}

// ErrNotFound is returned by Describe for a table or view that does not
// exist, or that the login cannot see.
var ErrNotFound = errors.New("no such table or view")

// cataloger is what each driver implements for the agent.
type cataloger interface {
	describe(ctx context.Context, conn *sql.DB, schema, name string) (*Table, error)
	search(ctx context.Context, conn *sql.DB, pattern string, limit int) ([]Match, error)
	foreignKeys(ctx context.Context, conn *sql.DB, schema, table string) ([]ForeignKey, error)
	version(ctx context.Context, conn *sql.DB) (string, error)
}

func catalogerFor(d Driver) cataloger {
	switch d {
	case Postgres:
		return postgres{}
	case SQLite:
		return sqlite{}
	case SQLServer:
		return sqlserver{}
	}
	return nil
}

// Describe describes a table or view of a database ("" for the default).
func (p *Pool) Describe(ctx context.Context, database, schema, name string) (*Table, error) {
	conn, err := p.DB(ctx, database)
	if err != nil {
		return nil, err
	}
	return catalogerFor(p.cfg.Driver).describe(ctx, conn, schema, name)
}

// Search finds tables, views, routines and columns whose names contain
// text, ignoring case; at most limit of them.
func (p *Pool) Search(ctx context.Context, database, text string, limit int) ([]Match, error) {
	conn, err := p.DB(ctx, database)
	if err != nil {
		return nil, err
	}
	return catalogerFor(p.cfg.Driver).search(ctx, conn, likePattern(text), limit)
}

// ServerVersion is the engine and its version, e.g. "PostgreSQL 17.2".
func (p *Pool) ServerVersion(ctx context.Context) (string, error) {
	conn, err := p.DB(ctx, "")
	if err != nil {
		return "", err
	}
	return catalogerFor(p.cfg.Driver).version(ctx, conn)
}

// likePattern matches names containing text: LIKE's wildcards in it are
// escaped with a backslash, as are SQL Server's brackets.
func likePattern(text string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `[`, `\[`)
	return "%" + strings.ToLower(r.Replace(text)) + "%"
}

// splitList splits a list aggregated with the unit separator (char 31),
// which no identifier contains.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x1f")
}

// queryMatches scans rows of (kind, schema, name, column, type).
func queryMatches(ctx context.Context, conn *sql.DB, query string, args ...any) ([]Match, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Match
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.Kind, &m.Schema, &m.Name, &m.Column, &m.Type); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// groupedKey is one row of a key listing: a constraint or index and one of
// its columns, in order.
type groupedKey struct {
	name, schema, table, refSchema, refTable string
	column, refColumn                        string
}

// groupForeignKeys folds per-column rows into foreign keys, in row order.
func groupForeignKeys(rows []groupedKey) []ForeignKey {
	var out []ForeignKey
	for _, r := range rows {
		n := len(out)
		if n == 0 || out[n-1].Name != r.name || out[n-1].Table != r.table || out[n-1].Schema != r.schema {
			out = append(out, ForeignKey{Name: r.name, Schema: r.schema, Table: r.table, RefSchema: r.refSchema, RefTable: r.refTable})
			n++
		}
		out[n-1].Columns = append(out[n-1].Columns, r.column)
		out[n-1].RefColumns = append(out[n-1].RefColumns, r.refColumn)
	}
	return out
}

// queryKeys scans rows of (name, schema, table, ref schema, ref table,
// column, ref column) into foreign keys.
func queryKeys(ctx context.Context, conn *sql.DB, query string, args ...any) ([]ForeignKey, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []groupedKey
	for rows.Next() {
		var k groupedKey
		if err := rows.Scan(&k.name, &k.schema, &k.table, &k.refSchema, &k.refTable, &k.column, &k.refColumn); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return groupForeignKeys(keys), rows.Err()
}

// foreignKeyObjects lists a table's foreign keys for the explorer.
func foreignKeyObjects(fks []ForeignKey, err error) ([]Object, error) {
	if err != nil {
		return nil, err
	}
	out := make([]Object, len(fks))
	for i, fk := range fks {
		out[i] = Object{Kind: KindForeignKey, Name: fk.Label()}
	}
	return out, nil
}

// KindName names a kind for the agent: "table", "materialized view", …
func KindName(k Kind) string {
	switch k {
	case KindTable:
		return "table"
	case KindView:
		return "view"
	case KindMatView:
		return "materialized view"
	case KindFunction:
		return "function"
	case KindProcedure:
		return "procedure"
	case KindSequence:
		return "sequence"
	case KindIndex:
		return "index"
	case KindTrigger:
		return "trigger"
	case KindColumn:
		return "column"
	case KindForeignKey:
		return "foreign key"
	case KindSchema:
		return "schema"
	case KindDatabase:
		return "database"
	}
	return fmt.Sprint(int(k))
}
