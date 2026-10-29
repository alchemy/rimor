package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// scriptsFor lists the scripts of an object, failing the test on error.
func scriptsFor(t *testing.T, p *Pool, o Object) map[string]Script {
	t.Helper()
	list, err := p.Scripts(context.Background(), o, 50)
	if err != nil {
		t.Fatalf("scripts for %s: %v", o.Name, err)
	}
	out := map[string]Script{}
	for _, s := range list {
		out[s.Name] = s
	}
	return out
}

// prepare checks that each script parses and names things that exist,
// without running it. Drops are commented out and checked as such.
func prepare(t *testing.T, conn *sql.DB, scripts map[string]Script) {
	t.Helper()
	for name, s := range scripts {
		if name == "drop" {
			for l := range strings.SplitSeq(strings.TrimSpace(s.SQL), "\n") {
				if !strings.HasPrefix(l, "--") {
					t.Errorf("drop runs: %q", l)
				}
			}
			continue
		}
		if s.Change != nil {
			continue // applied by ApplyChange: see change_test.go
		}
		stmt, err := conn.Prepare(s.SQL)
		if err != nil {
			t.Errorf("%s does not prepare: %v\n%s", name, err, s.SQL)
			continue
		}
		stmt.Close()
	}
}

func TestScriptsSQLite(t *testing.T) {
	p := sqliteOrders(t)
	ctx := context.Background()
	conn, err := p.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE "Order Notes" ("order" INTEGER, body TEXT NOT NULL DEFAULT '', words INT GENERATED ALWAYS AS (length(body)))`,
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	orders := scriptsFor(t, p, Object{Kind: KindTable, Name: "orders"})
	prepare(t, conn, orders)
	if got := orders["select"].SQL; got != "SELECT id, customer_id, delivery_date, status\nFROM orders\nLIMIT 50;\n" {
		t.Errorf("select:\n%s", got)
	}
	// The INTEGER PRIMARY KEY is assigned when left out, so the insert
	// leaves it out; it is the key for updates.
	want := "INSERT INTO orders (customer_id, delivery_date, status)\nVALUES (\n" +
		"    NULL, -- customer_id INTEGER, not null\n" +
		"    NULL, -- delivery_date DATE\n" +
		"    NULL  -- status VARCHAR(10)\n);\n"
	if got := orders["insert"].SQL; got != want {
		t.Errorf("insert:\n%s\nwant:\n%s", got, want)
	}
	want = "UPDATE orders\nSET customer_id = NULL,   -- customer_id INTEGER, not null\n" +
		"    delivery_date = NULL, -- delivery_date DATE\n" +
		"    status = NULL         -- status VARCHAR(10)\n" +
		"WHERE id = NULL; -- the row's key\n"
	if got := orders["update"].SQL; got != want {
		t.Errorf("update:\n%s\nwant:\n%s", got, want)
	}
	if !orders["delete"].Writes || orders["select"].Writes {
		t.Error("writes marked wrongly")
	}

	// Run as they are, the templates change nothing.
	for _, name := range []string{"update", "delete"} {
		res, err := conn.Exec(orders[name].SQL)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("%s as written changed %d rows", name, n)
		}
	}

	// Quoted names; a generated column is selected but never inserted; a
	// table without a primary key is updated by its rowid.
	notes := scriptsFor(t, p, Object{Kind: KindTable, Name: "Order Notes"})
	prepare(t, conn, notes)
	if got := notes["insert"].SQL; !strings.HasPrefix(got, `INSERT INTO "Order Notes" ("order", body)`) {
		t.Errorf("insert:\n%s", got)
	}
	if got := notes["select"].SQL; !strings.Contains(got, `"order", body, words`) {
		t.Errorf("select:\n%s", got)
	}
	if got := notes["update"].SQL; !strings.Contains(got, "WHERE rowid = NULL") {
		t.Errorf("update by rowid:\n%s", got)
	}

	// Composite keys.
	lines := scriptsFor(t, p, Object{Kind: KindTable, Name: "order_lines"})
	if got := lines["delete"].SQL; got != "DELETE FROM order_lines\nWHERE order_id = NULL\n  AND line_no = NULL; -- the row's key\n" {
		t.Errorf("delete:\n%s", got)
	}

	view := scriptsFor(t, p, Object{Kind: KindView, Name: "late_orders"})
	prepare(t, conn, view)
	if len(view) != 4 || view["change"].Change == nil {
		t.Errorf("view scripts: %v", view)
	}

	col := scriptsFor(t, p, Object{Kind: KindColumn, Name: "status", Table: "orders"})
	prepare(t, conn, col)

	fks := scriptsFor(t, p, Object{Kind: KindForeignKey, Name: "order_id → orders(id)", Table: "order_lines"})
	prepare(t, conn, fks)
	if got := fks["orphans"].SQL; got != "SELECT c.*\nFROM order_lines AS c\nLEFT JOIN orders AS p ON p.id = c.order_id\n"+
		"WHERE p.id IS NULL\n  AND c.order_id IS NOT NULL\nLIMIT 50;\n" {
		t.Errorf("orphans:\n%s", got)
	}

	idx := scriptsFor(t, p, Object{Kind: KindIndex, Name: "orders_due", Detail: "orders"})
	if got := idx["drop"].SQL; !strings.Contains(got, "-- DROP INDEX orders_due;") {
		t.Errorf("drop index:\n%s", got)
	}
}

func TestScriptsPostgres(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_POSTGRES")
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() { conn.Exec(`DROP SCHEMA IF EXISTS "Rimor Scripts" CASCADE`) }
	drop()
	t.Cleanup(func() { drop(); conn.Close() })
	for _, s := range []string{
		`CREATE SCHEMA "Rimor Scripts"`,
		`CREATE TABLE "Rimor Scripts"."Customers" (id serial PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE "Rimor Scripts".orders (
			id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			customer_id int NOT NULL REFERENCES "Rimor Scripts"."Customers",
			"order" text DEFAULT 'new',
			total numeric(10,2),
			doubled numeric GENERATED ALWAYS AS (total * 2) STORED)`,
		`CREATE SEQUENCE "Rimor Scripts".tickets`,
		`CREATE INDEX orders_customer ON "Rimor Scripts".orders (customer_id)`,
		`CREATE FUNCTION "Rimor Scripts".add(a int, b int DEFAULT 1, OUT total int) LANGUAGE sql AS 'select a + b'`,
		`CREATE FUNCTION "Rimor Scripts".rows_of(n int) RETURNS TABLE(k int) LANGUAGE sql AS 'select generate_series(1, n)'`,
		`CREATE PROCEDURE "Rimor Scripts".settle(INOUT done boolean, who character varying) LANGUAGE sql AS 'select true'`,
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}

	p := NewPool(Config{Driver: Postgres, DSN: dsn})
	defer p.Close()
	ctx := context.Background()
	db, err := p.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	const schema = "Rimor Scripts"

	orders := scriptsFor(t, p, Object{Kind: KindTable, Schema: schema, Name: "orders"})
	prepare(t, db, orders)
	// Identity and generated columns are not inserted.
	if got := orders["insert"].SQL; !strings.HasPrefix(got, `INSERT INTO "Rimor Scripts".orders (customer_id, "order", total)`) {
		t.Errorf("insert:\n%s", got)
	}
	if got := orders["select"].SQL; !strings.Contains(got, `SELECT id, customer_id, "order", total, doubled`) {
		t.Errorf("select:\n%s", got)
	}
	customers := scriptsFor(t, p, Object{Kind: KindTable, Schema: schema, Name: "Customers"})
	prepare(t, db, customers)
	if got := customers["insert"].SQL; !strings.HasPrefix(got, `INSERT INTO "Rimor Scripts"."Customers" (name)`) {
		t.Errorf("serial inserted:\n%s", got)
	}

	prepare(t, db, scriptsFor(t, p, Object{Kind: KindColumn, Schema: schema, Table: "orders", Name: "order"}))
	prepare(t, db, scriptsFor(t, p, Object{Kind: KindForeignKey, Schema: schema, Table: "orders", Name: `customer_id → Customers(id)`}))
	prepare(t, db, scriptsFor(t, p, Object{Kind: KindSequence, Schema: schema, Name: "tickets"}))

	// Routines come from the explorer, where the kind and the overload are.
	folder := Object{Kind: KindFolder, Contains: KindFunction, Schema: schema}
	routines, err := p.Children(ctx, &folder)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]string{}
	for _, o := range routines {
		s := scriptsFor(t, p, o)["call"]
		calls[o.Name] = s.SQL
		if o.Kind == KindProcedure != s.Writes {
			t.Errorf("%s: writes %v", o.Name, s.Writes)
		}
	}
	want := map[string]string{
		"add":     "SELECT *\nFROM \"Rimor Scripts\".\"add\"(\n    NULL, -- a integer\n    NULL  -- b integer\n);\n",
		"rows_of": "SELECT *\nFROM \"Rimor Scripts\".rows_of(\n    NULL  -- n integer\n);\n",
		"settle":  "CALL \"Rimor Scripts\".settle(\n    NULL, -- done boolean, output\n    NULL  -- who character varying\n);\n",
	}
	for name, w := range want {
		if calls[name] != w {
			t.Errorf("call %s:\n%s\nwant:\n%s", name, calls[name], w)
		}
	}
	// The calls run: they change nothing.
	for name, sql := range calls {
		if _, err := db.Exec(sql); err != nil {
			t.Errorf("call %s: %v", name, err)
		}
	}
}

// TestScriptsSQLServer only compiles (SET NOEXEC ON), inside a transaction
// that is rolled back, against a table the database already has.
func TestScriptsSQLServer(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_SQLSERVER")
	ctx := context.Background()
	p := NewPool(Config{Driver: SQLServer, DSN: dsn})
	defer p.Close()
	db, err := p.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var schema, table string
	err = db.QueryRowContext(ctx, `
		SELECT TOP 1 s.name, t.name FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id
		WHERE t.is_ms_shipped = 0 AND EXISTS (SELECT 1 FROM sys.foreign_keys fk WHERE fk.parent_object_id = t.object_id)
		ORDER BY s.name, t.name`).Scan(&schema, &table)
	if err != nil {
		t.Skip("no table with a foreign key: ", err)
	}
	tb, err := p.Describe(ctx, "", schema, table)
	if err != nil {
		t.Fatal(err)
	}

	check := func(scripts map[string]Script) {
		for name, s := range scripts {
			if name == "drop" {
				continue
			}
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.ExecContext(ctx, "BEGIN TRANSACTION; SET NOEXEC ON;\n"+s.SQL+"\nSET NOEXEC OFF; ROLLBACK;")
			if err != nil {
				t.Errorf("%s does not compile: %v\n%s", name, err, s.SQL)
			}
			c.Close()
		}
	}
	// The check itself: compiling catches a wrong name.
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ExecContext(ctx, "BEGIN TRANSACTION; SET NOEXEC ON;\nSELECT rimor_no_such_column FROM "+
		quoter{SQLServer}.qualified(schema, table)+";\nSET NOEXEC OFF; ROLLBACK;")
	c.Close()
	if err == nil {
		t.Fatal("NOEXEC compiled a query naming a missing column: the checks below prove nothing")
	}

	check(scriptsFor(t, p, Object{Kind: KindTable, Schema: schema, Name: table}))
	check(scriptsFor(t, p, Object{Kind: KindColumn, Schema: schema, Table: table, Name: tb.Columns[0].Name}))
	check(scriptsFor(t, p, Object{Kind: KindForeignKey, Schema: schema, Table: table, Name: tb.ForeignKeys[0].Label()}))

	var proc string
	if db.QueryRowContext(ctx, `SELECT TOP 1 name FROM sys.procedures WHERE SCHEMA_NAME(schema_id) = 'dbo' AND is_ms_shipped = 0 ORDER BY name`).Scan(&proc) == nil {
		s := scriptsFor(t, p, Object{Kind: KindProcedure, Schema: "dbo", Name: proc})["call"]
		if !strings.HasPrefix(s.SQL, "EXEC ") && !strings.HasPrefix(s.SQL, "DECLARE ") {
			t.Errorf("call:\n%s", s.SQL)
		}
		check(map[string]Script{"call": s})
	}
}
