package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func pgSession(t *testing.T, cfg Config) *sql.Conn {
	t.Helper()
	pool := NewPool(cfg)
	t.Cleanup(pool.Close)
	s, err := pool.Session(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustExec(t *testing.T, conn *sql.Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestEditPostgres(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_POSTGRES")
	ctx := context.Background()
	conn := pgSession(t, Config{Driver: Postgres, DSN: dsn})
	mustExec(t, conn,
		`DROP TABLE IF EXISTS edit_items, edit_nokey`,
		`CREATE TABLE edit_items (id int PRIMARY KEY, name text, price numeric(10,2),
			seen timestamptz, total numeric GENERATED ALWAYS AS (price * 2) STORED)`,
		`INSERT INTO edit_items VALUES (1, 'bolt', 1.50, '2026-10-01 12:00:00+02'), (2, 'nut', NULL, NULL)`,
		`CREATE TABLE edit_nokey (a int)`,
	)

	// A plain SELECT: id is the key, total is generated, the expression is not a column.
	o, err := FindOrigin(ctx, conn, Postgres, "SELECT id, name, price, seen, total, upper(name) AS shout FROM edit_items", 6)
	if err != nil {
		t.Fatal(err)
	}
	if o.Table != "public.edit_items" {
		t.Errorf("table = %q", o.Table)
	}
	for col, want := range map[int]string{0: "Key columns", 4: "computes", 5: "not a column"} {
		if err := o.Editable(col); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("column %d: %v, want %q", col, err, want)
		}
	}
	for _, col := range []int{1, 2, 3} {
		if err := o.Editable(col); err != nil {
			t.Errorf("column %d: %v", col, err)
		}
	}

	// Edit name of row 1: read, update, reload.
	cell := Cell{Origin: o, Col: 1, Key: []string{"1"}}
	cur, err := cell.Current(ctx, conn)
	if err != nil || cur == nil || *cur != "bolt" {
		t.Fatalf("current = %v, %v", cur, err)
	}
	v := "hex bolt"
	got, err := cell.Update(ctx, conn, cur, &v)
	if err != nil || got.Text != "hex bolt" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	if p := cell.Preview(&v); p != "UPDATE public.edit_items SET name = 'hex bolt' WHERE id = '1'" {
		t.Errorf("preview = %q", p)
	}

	// A typed column, converted by the database; then NULL.
	price := Cell{Origin: o, Col: 2, Key: []string{"2"}}
	old, _ := price.Current(ctx, conn)
	if old != nil {
		t.Fatalf("price of row 2 should be NULL, is %q", *old)
	}
	ten := "10.5"
	if got, err := price.Update(ctx, conn, old, &ten); err != nil || got.Text != "10.50" {
		t.Fatalf("price = %+v, %v", got, err)
	}
	old, _ = price.Current(ctx, conn)
	if got, err := price.Update(ctx, conn, old, nil); err != nil || !got.Null {
		t.Fatalf("set NULL = %+v, %v", got, err)
	}
	bad := "ten"
	if _, err := price.Update(ctx, conn, nil, &bad); err == nil {
		t.Error("an invalid number was accepted")
	}

	// A timestamptz keeps its exact text, so an edit from it matches.
	seen := Cell{Origin: o, Col: 3, Key: []string{"1"}}
	old, _ = seen.Current(ctx, conn)
	if old == nil || !strings.Contains(*old, "2026-10-01") {
		t.Fatalf("seen = %v", old)
	}
	later := "2026-10-02 08:00:00+00"
	if _, err := seen.Update(ctx, conn, old, &later); err != nil {
		t.Fatalf("seen update: %v", err)
	}

	// Someone else changed the row: the stale edit is refused.
	cur, _ = cell.Current(ctx, conn)
	mustExec(t, conn, `UPDATE edit_items SET name = 'changed elsewhere' WHERE id = 1`)
	again := "mine"
	if _, err := cell.Update(ctx, conn, cur, &again); !errors.Is(err, ErrRowChanged) {
		t.Errorf("stale edit: %v", err)
	}

	// Results that cannot be edited.
	for query, want := range map[string]string{
		"SELECT name, price FROM edit_items":                                 "Add the key",
		"SELECT a FROM edit_nokey":                                           "no primary key",
		"SELECT i.id, n.a FROM edit_items i JOIN edit_nokey n ON n.a = i.id": "several tables",
		"SELECT 1 AS one":                                                    "No column",
	} {
		cols := strings.Count(strings.SplitN(query, "FROM", 2)[0], ",") + 1
		if _, err := FindOrigin(ctx, conn, Postgres, query, cols); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", query, err, want)
		}
	}
	mustExec(t, conn, `DROP TABLE edit_items, edit_nokey`)
}

func TestEditSQLServer(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_SQLSERVER")
	ctx := context.Background()
	conn := pgSession(t, Config{Driver: SQLServer, DSN: dsn})
	// A temp table cannot be described, so use a real one and drop it.
	mustExec(t, conn,
		`IF OBJECT_ID('dbo.rimor_edit_test') IS NOT NULL DROP TABLE dbo.rimor_edit_test`,
		`CREATE TABLE dbo.rimor_edit_test (id int IDENTITY PRIMARY KEY, code nvarchar(20) NOT NULL,
			price decimal(10,2) NULL, seen datetime2 NULL, twice AS (price * 2))`,
		`INSERT INTO dbo.rimor_edit_test (code, price, seen) VALUES (N'bolt', 1.5, '2026-10-01T12:00:00.1234567')`,
	)
	defer mustExec(t, conn, `DROP TABLE dbo.rimor_edit_test`)

	o, err := FindOrigin(ctx, conn, SQLServer, "SELECT id, code, price, seen, twice FROM dbo.rimor_edit_test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Editable(4); err == nil || !strings.Contains(err.Error(), "computes") {
		t.Errorf("computed column: %v", err)
	}
	var id string
	conn.QueryRowContext(ctx, `SELECT CAST(MAX(id) AS nvarchar) FROM dbo.rimor_edit_test`).Scan(&id)

	cell := Cell{Origin: o, Col: 1, Key: []string{id}}
	cur, err := cell.Current(ctx, conn)
	if err != nil || *cur != "bolt" {
		t.Fatalf("current = %v, %v", cur, err)
	}
	v := "nut"
	if got, err := cell.Update(ctx, conn, cur, &v); err != nil || got.Text != "nut" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	// datetime2 keeps all seven digits through its exact text.
	seen := Cell{Origin: o, Col: 3, Key: []string{id}}
	old, err := seen.Current(ctx, conn)
	if err != nil || old == nil || !strings.Contains(*old, ".1234567") {
		t.Fatalf("seen = %v, %v", old, err)
	}
	later := "2026-10-02T08:00:00"
	if _, err := seen.Update(ctx, conn, old, &later); err != nil {
		t.Fatalf("seen update: %v", err)
	}
	if _, err := FindOrigin(ctx, conn, SQLServer, "SELECT code FROM dbo.rimor_edit_test", 1); err == nil ||
		!strings.Contains(err.Error(), "Add the key") {
		t.Errorf("without key: %v", err)
	}
}

func TestReadOnly(t *testing.T) {
	ctx := context.Background()

	// SQLite: the engine refuses writes.
	path := filepath.Join(t.TempDir(), "ro.db")
	rw, _ := sql.Open("sqlite", path)
	rw.Exec("CREATE TABLE t (a int)")
	rw.Close()
	s := pgSession(t, Config{Driver: SQLite, DSN: path, ReadOnly: true})
	if _, err := s.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err == nil {
		t.Error("SQLite read-only connection wrote")
	}
	if _, err := s.QueryContext(ctx, "SELECT * FROM t"); err != nil {
		t.Errorf("SQLite read-only connection cannot read: %v", err)
	}

	// PostgreSQL: the server refuses writes.
	if dsn := serverDSN(t, "RIMOR_TEST_POSTGRES"); dsn != "" {
		pg := pgSession(t, Config{Driver: Postgres, DSN: dsn, ReadOnly: true})
		if _, err := pg.ExecContext(ctx, "CREATE TABLE ro_test (a int)"); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("Postgres read-only connection: %v", err)
		}
	}
}

func TestCountName(t *testing.T) {
	for query, want := range map[string]int{
		"SELECT * FROM items": 1,
		"SELECT i.id FROM items i JOIN items j ON j.id = i.id":         2,
		`SELECT * FROM "Items" WHERE note = 'items' -- items`:          1,
		"SELECT * FROM dbo.[items] WHERE id IN (SELECT id FROM items)": 2,
		"SELECT items_count FROM stock":                                0,
	} {
		if got := countName(query, "items"); got != want {
			t.Errorf("%q: %d, want %d", query, got, want)
		}
	}
}

func TestCheckReadOnly(t *testing.T) {
	for query, ok := range map[string]bool{
		"SELECT * FROM t":                       true,
		"with x as (select 1) select * from x":  true,
		"SELECT 'DELETE FROM t' AS s -- DROP":   true,
		"SELECT [update] FROM t":                true,
		"/* INSERT */ SELECT 1":                 true,
		"UPDATE t SET a = 1":                    false,
		"select * into #copy from t":            false,
		"EXEC dbo.do_things":                    false,
		"SELECT 1; DROP TABLE t":                false,
		"WITH x AS (SELECT 1) DELETE FROM t":    false,
		"SELECT 'it''s' AS s; truncate table t": false,
	} {
		if err := CheckReadOnly(query); (err == nil) != ok {
			t.Errorf("%q: %v", query, err)
		}
	}
}

func TestEditSQLServerISJSON(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_SQLSERVER")
	ctx := context.Background()
	conn := pgSession(t, Config{Driver: SQLServer, DSN: dsn})
	mustExec(t, conn,
		`IF OBJECT_ID('dbo.rimor_json_test') IS NOT NULL DROP TABLE dbo.rimor_json_test`,
		`CREATE TABLE dbo.rimor_json_test (id int PRIMARY KEY,
			doc nvarchar(max) CONSTRAINT rimor_doc_json CHECK (ISJSON(doc) = 1),
			other nvarchar(max), plain nvarchar(100),
			CONSTRAINT rimor_other_json CHECK (ISJSON(other) > 0 AND len(other) < 4000))`,
		`INSERT INTO dbo.rimor_json_test VALUES (1, N'{"a":1}', N'[1,2]', N'x')`,
	)
	defer mustExec(t, conn, `DROP TABLE dbo.rimor_json_test`)

	o, err := FindOrigin(ctx, conn, SQLServer, "SELECT id, doc, other, plain FROM dbo.rimor_json_test", 4)
	if err != nil {
		t.Fatal(err)
	}
	for col, want := range map[int]bool{0: false, 1: true, 2: true, 3: false} {
		if got := o.IsJSON(col); got != want {
			t.Errorf("column %d: IsJSON = %v, want %v", col, got, want)
		}
	}
	// A disabled check does not count.
	mustExec(t, conn, `ALTER TABLE dbo.rimor_json_test NOCHECK CONSTRAINT rimor_doc_json`)
	o, _ = FindOrigin(ctx, conn, SQLServer, "SELECT id, doc FROM dbo.rimor_json_test", 2)
	if o.IsJSON(1) {
		t.Error("disabled ISJSON check still counted")
	}
}
