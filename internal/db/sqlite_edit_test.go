package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// sqliteEditDB creates a database with the shapes editing must handle.
func sqliteEditDB(t *testing.T) (*sql.Conn, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edit.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	for _, s := range []string{
		`CREATE TABLE items (
			id INTEGER PRIMARY KEY, name TEXT, qty INTEGER, price REAL,
			doc JSON, bin JSONB,
			checked TEXT CHECK (json_valid(checked)),
			packed BLOB CHECK (json_valid(packed, 8)),
			total REAL GENERATED ALWAYS AS (qty * price) VIRTUAL,
			loose)`, // no declared type: values keep the type they are given
		`INSERT INTO items (id, name, qty, price, doc, bin, checked, packed, loose)
			VALUES (1, 'bolt', 5, 1.5, '{"a":1}', jsonb('{"b":[1,2]}'), '[1]', jsonb('{"c":true}'), 5)`,
		`CREATE TABLE nokey (a TEXT, b TEXT)`,
		`INSERT INTO nokey VALUES ('x', 'y')`,
		`CREATE TABLE coded (code TEXT NOT NULL, label TEXT)`,
		`CREATE UNIQUE INDEX coded_code ON coded(code)`,
		`INSERT INTO coded VALUES ('A1', 'first')`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return pgSession(t, Config{Driver: SQLite, DSN: path}), raw
}

func TestEditSQLite(t *testing.T) {
	ctx := context.Background()
	conn, raw := sqliteEditDB(t)

	o, err := FindOrigin(ctx, conn, SQLite,
		"SELECT id, name, qty, price, doc, bin, checked, packed, total, name || '!' AS shout FROM items", 10)
	if err != nil {
		t.Fatal(err)
	}
	for col, want := range map[int]string{0: "Key columns", 8: "computes", 9: "not a column"} {
		if err := o.Editable(col); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("column %d: %v, want %q", col, err, want)
		}
	}
	// JSON by declared type and by json_valid checks.
	for col, want := range map[int][2]bool{4: {true, false}, 5: {false, true}, 6: {true, false}, 7: {false, true}, 1: {false, false}} {
		if got := [2]bool{o.IsJSON(col), o.IsJSONB(col)}; got != want {
			t.Errorf("column %d: json, jsonb = %v, want %v", col, got, want)
		}
	}

	// Text, then a number typed as text: the column's affinity stores 42.
	name := Cell{Origin: o, Col: 1, Key: []string{"1"}}
	cur, err := name.Current(ctx, conn)
	if err != nil || *cur.Text != "bolt" {
		t.Fatalf("current = %+v, %v", cur, err)
	}
	v := "hex bolt"
	if got, err := name.Update(ctx, conn, cur, &v); err != nil || got.Text != "hex bolt" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	qty := Cell{Origin: o, Col: 2, Key: []string{"1"}}
	cur, _ = qty.Current(ctx, conn)
	n := "42"
	if _, err := qty.Update(ctx, conn, cur, &n); err != nil {
		t.Fatal(err)
	}
	var typ string
	raw.QueryRow("SELECT typeof(qty) FROM items WHERE id = 1").Scan(&typ)
	if typ != "integer" {
		t.Errorf("qty stored as %s", typ)
	}

	// In a column without a declared type, the integer 5 became the text
	// '5' behind rimor's back: the same text, a different value.
	o2, err := FindOrigin(ctx, conn, SQLite, "SELECT id, loose FROM items", 2)
	if err != nil {
		t.Fatal(err)
	}
	loose := Cell{Origin: o2, Col: 1, Key: []string{"1"}}
	cur, _ = loose.Current(ctx, conn)
	if _, err := raw.Exec("UPDATE items SET loose = '5' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	seven := "7"
	if _, err := loose.Update(ctx, conn, cur, &seven); !errors.Is(err, ErrRowChanged) {
		t.Errorf("a change of type was not detected: %v", err)
	}

	// JSONB: edited as text, stored binary.
	bin := Cell{Origin: o, Col: 5, Key: []string{"1"}}
	cur, err = bin.Current(ctx, conn)
	if err != nil || *cur.Text != `{"b":[1,2]}` {
		t.Fatalf("jsonb current = %+v, %v", cur, err)
	}
	doc := `{"b": [3]}`
	if _, err := bin.Update(ctx, conn, cur, &doc); err != nil {
		t.Fatal(err)
	}
	var back, btype string
	raw.QueryRow("SELECT json(bin), typeof(bin) FROM items WHERE id = 1").Scan(&back, &btype)
	if back != `{"b":[3]}` || btype != "blob" {
		t.Errorf("jsonb stored %q as %s", back, btype)
	}
	if p := bin.Preview(&doc); !strings.Contains(p, `SET bin = jsonb('{"b": [3]}')`) {
		t.Errorf("preview = %q", p)
	}
	// NULL, and the json_valid check rejecting a bad value.
	checked := Cell{Origin: o, Col: 6, Key: []string{"1"}}
	cur, _ = checked.Current(ctx, conn)
	bad := "not json"
	if _, err := checked.Update(ctx, conn, cur, &bad); err == nil {
		t.Error("json_valid check did not reject")
	}
	if got, err := checked.Update(ctx, conn, cur, nil); err != nil || !got.Null {
		t.Errorf("set NULL = %+v, %v", got, err)
	}

	// Someone else changed the row.
	cur, _ = name.Current(ctx, conn)
	raw.Exec("UPDATE items SET name = 'elsewhere' WHERE id = 1")
	mine := "mine"
	if _, err := name.Update(ctx, conn, cur, &mine); !errors.Is(err, ErrRowChanged) {
		t.Errorf("stale edit: %v", err)
	}
}

func TestEditSQLiteKeys(t *testing.T) {
	ctx := context.Background()
	conn, raw := sqliteEditDB(t)

	// No primary key: the rowid, when selected, finds the row.
	if _, err := FindOrigin(ctx, conn, SQLite, "SELECT a, b FROM nokey", 2); err == nil || !strings.Contains(err.Error(), "add rowid") {
		t.Errorf("without rowid: %v", err)
	}
	o, err := FindOrigin(ctx, conn, SQLite, "SELECT rowid, a, b FROM nokey", 3)
	if err != nil {
		t.Fatal(err)
	}
	cell := Cell{Origin: o, Col: 2, Key: []string{"1"}}
	cur, _ := cell.Current(ctx, conn)
	z := "z"
	if _, err := cell.Update(ctx, conn, cur, &z); err != nil {
		t.Fatal(err)
	}
	var b string
	raw.QueryRow("SELECT b FROM nokey").Scan(&b)
	if b != "z" {
		t.Errorf("b = %q", b)
	}

	// A unique index on a non-null column.
	o, err = FindOrigin(ctx, conn, SQLite, "SELECT code, label FROM coded", 2)
	if err != nil {
		t.Fatal(err)
	}
	label := Cell{Origin: o, Col: 1, Key: []string{"A1"}}
	cur, _ = label.Current(ctx, conn)
	second := "second"
	if _, err := label.Update(ctx, conn, cur, &second); err != nil {
		t.Fatal(err)
	}

	// Refused shapes.
	for query, want := range map[string]string{
		"SELECT i.id, j.name FROM items i JOIN items j ON j.id = i.id": "appears 2 times",
		"SELECT i.id, n.a FROM items i JOIN nokey n":                   "several tables",
		"SELECT name FROM items":                                       "Add the key",
	} {
		cols := strings.Count(strings.SplitN(query, "FROM", 2)[0], ",") + 1
		if _, err := FindOrigin(ctx, conn, SQLite, query, cols); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", query, err, want)
		}
	}
}
