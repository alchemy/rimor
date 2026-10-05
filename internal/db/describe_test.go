package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ordersSchema is a small ERP-like schema, valid on all three engines but
// for the column types each gets.
var ordersSchema = []string{
	`CREATE TABLE customers (id INTEGER PRIMARY KEY, name VARCHAR(80) NOT NULL, region VARCHAR(20) DEFAULT 'north')`,
	`CREATE TABLE orders (
		id INTEGER PRIMARY KEY,
		customer_id INTEGER NOT NULL REFERENCES customers(id),
		delivery_date DATE,
		status VARCHAR(10)
	)`,
	`CREATE INDEX orders_due ON orders (delivery_date, status)`,
	`CREATE TABLE order_lines (
		order_id INTEGER NOT NULL,
		line_no INTEGER NOT NULL,
		qty INTEGER,
		PRIMARY KEY (order_id, line_no),
		FOREIGN KEY (order_id) REFERENCES orders(id)
	)`,
	`CREATE VIEW late_orders AS SELECT id, delivery_date FROM orders`,
}

func sqliteOrders(t *testing.T) *Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "erp.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range ordersSchema {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	// A key that names no columns refers to the primary key.
	if _, err := conn.Exec(`CREATE TABLE notes (order_id INTEGER REFERENCES orders, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	p := NewPool(Config{Driver: SQLite, DSN: path})
	t.Cleanup(p.Close)
	return p
}

// render writes a description compactly for comparisons.
func render(tb *Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s.%s\n", tb.Kind, tb.Schema, tb.Name)
	for _, c := range tb.Columns {
		fmt.Fprintf(&b, "  %s %s", c.Name, c.Type)
		if !c.Nullable {
			b.WriteString(" not null")
		}
		if c.PrimaryKey {
			b.WriteString(" pk")
		}
		if c.Implicit {
			b.WriteString(" implicit")
		}
		if c.Default != "" {
			b.WriteString(" default " + c.Default)
		}
		if c.Comment != "" {
			b.WriteString(" -- " + c.Comment)
		}
		b.WriteString("\n")
	}
	for _, x := range tb.Indexes {
		fmt.Fprintf(&b, "  index (%s)", strings.Join(x.Columns, ", "))
		if x.Unique {
			b.WriteString(" unique")
		}
		if x.Primary {
			b.WriteString(" primary")
		}
		b.WriteString("\n")
	}
	for _, fk := range tb.ForeignKeys {
		fmt.Fprintf(&b, "  fk %s\n", fk.Label())
	}
	for _, fk := range tb.ReferencedBy {
		fmt.Fprintf(&b, "  from %s.%s(%s)\n", fk.Schema, fk.Table, strings.Join(fk.Columns, ", "))
	}
	return b.String()
}

func TestDescribeSQLite(t *testing.T) {
	p := sqliteOrders(t)
	ctx := context.Background()

	tb, err := p.Describe(ctx, "", "", "ORDERS") // names ignore case, as in SQLite
	if err != nil {
		t.Fatal(err)
	}
	got := render(tb)
	want := `table .orders
  id INTEGER not null pk
  customer_id INTEGER not null
  delivery_date DATE
  status VARCHAR(10)
  index (delivery_date, status)
  fk customer_id → customers(id)
  from .notes(order_id)
  from .order_lines(order_id)
`
	if got != want {
		t.Errorf("orders:\n%s\nwant:\n%s", got, want)
	}

	tb, err = p.Describe(ctx, "", "", "customers")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(render(tb), "region VARCHAR(20) default 'north'") {
		t.Errorf("default missing:\n%s", render(tb))
	}

	// A composite key, and the implicit rowid of a table without an
	// INTEGER PRIMARY KEY.
	tb, err = p.Describe(ctx, "", "", "order_lines")
	if err != nil {
		t.Fatal(err)
	}
	got = render(tb)
	for _, w := range []string{"  rowid INTEGER not null implicit\n", "  index (order_id, line_no) unique primary\n", "  fk order_id → orders(id)\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("order_lines lacks %q:\n%s", w, got)
		}
	}

	tb, err = p.Describe(ctx, "", "", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if got := render(tb); !strings.Contains(got, "fk order_id → orders(id)") {
		t.Errorf("implicit reference not resolved:\n%s", got)
	}

	tb, err = p.Describe(ctx, "", "", "late_orders")
	if err != nil || tb.Kind != "view" || len(tb.Columns) != 2 {
		t.Errorf("view: %+v %v", tb, err)
	}

	if _, err := p.Describe(ctx, "", "", "nope"); err != ErrNotFound {
		t.Errorf("missing table: %v", err)
	}
}

func matchNames(ms []Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Kind + " " + m.Name
		if m.Column != "" {
			out[i] += "." + m.Column
		}
	}
	return out
}

func TestSearchSQLite(t *testing.T) {
	p := sqliteOrders(t)
	ctx := context.Background()

	ms, err := p.Search(ctx, "", "ORDER", 50)
	if err != nil {
		t.Fatal(err)
	}
	got := matchNames(ms)
	want := []string{"view late_orders", "table order_lines", "table orders", "column notes.order_id", "column order_lines.order_id"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	ms, _ = p.Search(ctx, "", "deliv", 50)
	if got := matchNames(ms); !slices.Equal(got, []string{"column late_orders.delivery_date", "column orders.delivery_date"}) {
		t.Errorf("deliv: %q", got)
	}
	if ms[1].Type != "DATE" {
		t.Errorf("column type %q", ms[1].Type)
	}

	// Wildcards are literal.
	if ms, _ := p.Search(ctx, "", "%", 50); len(ms) != 0 {
		t.Errorf("%% matched %q", matchNames(ms))
	}
	if ms, _ := p.Search(ctx, "", "r_l", 50); !slices.Equal(matchNames(ms), []string{"table order_lines"}) {
		t.Errorf("r_l matched %q", matchNames(ms))
	}

	if ms, _ := p.Search(ctx, "", "o", 3); len(ms) != 3 {
		t.Errorf("limit: %d matches", len(ms))
	}
	if v, err := p.ServerVersion(ctx); err != nil || !strings.HasPrefix(v, "SQLite 3.") {
		t.Errorf("version %q %v", v, err)
	}
}

func TestForeignKeysInExplorer(t *testing.T) {
	p := sqliteOrders(t)
	got := walk(t, p.Config(), false)
	requireLines(t, got,
		"  orders",
		"    Foreign Keys",
		"      customer_id → customers(id)",
	)
}

func TestDescribePostgres(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_POSTGRES")
	ctx := context.Background()
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		conn.Exec(`DROP SCHEMA IF EXISTS rimor_describe2 CASCADE`)
		conn.Exec(`DROP SCHEMA IF EXISTS rimor_describe CASCADE`)
	}
	drop()
	t.Cleanup(func() { drop(); conn.Close() })
	stmts := append([]string{`CREATE SCHEMA rimor_describe`, `SET search_path = rimor_describe`}, ordersSchema...)
	stmts = append(stmts,
		`COMMENT ON TABLE orders IS 'Customer orders'`,
		`COMMENT ON COLUMN orders.delivery_date IS 'Promised delivery'`,
		`CREATE SCHEMA rimor_describe2`,
		`CREATE TABLE rimor_describe2.invoices (id int PRIMARY KEY, order_id int REFERENCES rimor_describe.orders(id))`,
	)
	c, err := conn.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if _, err := c.ExecContext(ctx, s); err != nil {
			t.Fatal(s, err)
		}
	}
	c.Close()

	p := NewPool(Config{Driver: Postgres, DSN: dsn})
	defer p.Close()
	tb, err := p.Describe(ctx, "", "rimor_describe", "orders")
	if err != nil {
		t.Fatal(err)
	}
	got := render(tb)
	want := `table rimor_describe.orders
  id integer not null pk
  customer_id integer not null
  delivery_date date -- Promised delivery
  status character varying(10)
  index (delivery_date, status)
  index (id) unique primary
  fk customer_id → customers(id)
  from rimor_describe.order_lines(order_id)
  from rimor_describe2.invoices(order_id)
`
	if got != want || tb.Comment != "Customer orders" {
		t.Errorf("got:\n%s\nwant:\n%s\ncomment %q", got, want, tb.Comment)
	}

	tb, err = p.Describe(ctx, "", "rimor_describe2", "invoices")
	if err != nil {
		t.Fatal(err)
	}
	if fk := tb.ForeignKeys; len(fk) != 1 || fk[0].Label() != "order_id → rimor_describe.orders(id)" {
		t.Errorf("cross-schema key: %+v", fk)
	}

	ms, err := p.Search(ctx, "", "deliv", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := matchNames(ms); !slices.Contains(got, "column orders.delivery_date") || !slices.Contains(got, "column late_orders.delivery_date") {
		t.Errorf("search: %q", got)
	}
	if v, err := p.ServerVersion(ctx); err != nil || !strings.HasPrefix(v, "PostgreSQL ") {
		t.Errorf("version %q %v", v, err)
	}
	if _, err := p.Describe(ctx, "", "rimor_describe", "nope"); err != ErrNotFound {
		t.Errorf("missing: %v", err)
	}

	// Descriptions are what the agent receives.
	if _, err := json.Marshal(tb); err != nil {
		t.Fatal(err)
	}
}

// TestDescribeSQLServer only reads: it describes a table the database
// already has, preferably one with a foreign key, and checks the result
// against the catalog.
func TestDescribeSQLServer(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_SQLSERVER")
	ctx := context.Background()
	conn, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var schema, table string
	var columns, keys int
	err = conn.QueryRowContext(ctx, `
		SELECT TOP 1 s.name, t.name,
		       (SELECT COUNT(*) FROM sys.columns c WHERE c.object_id = t.object_id),
		       (SELECT COUNT(*) FROM sys.foreign_keys fk WHERE fk.parent_object_id = t.object_id)
		FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id
		WHERE t.is_ms_shipped = 0
		ORDER BY CASE WHEN EXISTS (SELECT 1 FROM sys.foreign_keys fk WHERE fk.parent_object_id = t.object_id) THEN 0 ELSE 1 END,
		         s.name, t.name`).Scan(&schema, &table, &columns, &keys)
	if errors.Is(err, sql.ErrNoRows) {
		t.Skip("the database has no tables")
	}
	if err != nil {
		t.Fatal(err)
	}

	p := NewPool(Config{Driver: SQLServer, DSN: dsn})
	defer p.Close()
	tb, err := p.Describe(ctx, "", schema, table)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + render(tb))
	if tb.Kind != "table" || len(tb.Columns) != columns || len(tb.ForeignKeys) != keys {
		t.Errorf("%s.%s: %s, %d columns (want %d), %d foreign keys (want %d)",
			schema, table, tb.Kind, len(tb.Columns), columns, len(tb.ForeignKeys), keys)
	}
	for _, fk := range tb.ForeignKeys {
		if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefColumns) || fk.RefTable == "" {
			t.Errorf("foreign key %+v", fk)
		}
	}

	ms, err := p.Search(ctx, "", table, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range ms {
		found = found || (m.Kind == "table" && m.Schema == schema && m.Name == table)
	}
	if !found {
		t.Errorf("search for %q: %q", table, matchNames(ms))
	}
	if v, err := p.ServerVersion(ctx); err != nil || !strings.HasPrefix(v, "SQL Server ") {
		t.Errorf("version %q %v", v, err)
	}
	if _, err := p.Describe(ctx, "", schema, "rimor_no_such_table"); err != ErrNotFound {
		t.Errorf("missing: %v", err)
	}
}
