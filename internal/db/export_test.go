package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// exported runs ExportQuery and collects what it hands over.
func exported(t *testing.T, s *sql.Conn, driver Driver, query string, set int) ([]ExportColumn, [][]Value) {
	t.Helper()
	var cols []ExportColumn
	var rows [][]Value
	err := ExportQuery(context.Background(), s, driver, query, set,
		func(c []ExportColumn) error { cols = c; return nil },
		func(v []Value) error { rows = append(rows, append([]Value(nil), v...)); return nil })
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return cols, rows
}

func TestExportQuerySQLite(t *testing.T) {
	s := sqliteSession(t)
	cols, rows := exported(t, s, SQLite, "SELECT 1 AS n, zeroblob(40) AS b, NULL AS z", 0)
	if len(cols) != 3 || cols[0].Name != "n" || len(rows) != 1 {
		t.Fatalf("%+v %+v", cols, rows)
	}
	// Binary values whole, where the grid cuts them after 32 bytes.
	if b := rows[0][1].Text; len(b) != 2+80 || strings.HasSuffix(b, "…") {
		t.Errorf("binary %q", b)
	}
	if !rows[0][2].Null {
		t.Error("NULL lost")
	}
	if err := ExportQuery(context.Background(), s, SQLite, "CREATE TABLE y (a int)", 0,
		func([]ExportColumn) error { return nil }, func([]Value) error { return nil }); err != ErrNoResult {
		t.Errorf("no result: %v", err)
	}
}

func TestExportQueryServers(t *testing.T) {
	for _, c := range []struct {
		env    string
		driver Driver
		batch  string // a statement without rows, then two results
	}{
		{"RIMOR_TEST_POSTGRES", Postgres, ""},
		{"RIMOR_TEST_SQLSERVER", SQLServer, "DECLARE @n int = 2; SELECT TOP (@n) name FROM sys.objects ORDER BY name; SELECT 7 AS seven"},
		{"RIMOR_TEST_MYSQL", MySQL, "SET @n := 2; SELECT SCHEMA_NAME AS name FROM information_schema.SCHEMATA LIMIT 2; SELECT 7 AS seven"},
		{"RIMOR_TEST_MARIADB", MySQL, "SET @n := 2; SELECT SCHEMA_NAME AS name FROM information_schema.SCHEMATA LIMIT 2; SELECT 7 AS seven"},
	} {
		t.Run(strings.ToLower(strings.TrimPrefix(c.env, "RIMOR_TEST_")), func(t *testing.T) {
			p := NewPool(Config{Driver: c.driver, DSN: serverDSN(t, c.env)})
			defer p.Close()
			s, err := p.Session(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Release(s)
			cols, rows := exported(t, s, c.driver, "SELECT 1 AS one, 'x' AS two", 0)
			if len(cols) != 2 || len(rows) != 1 || rows[0][1].Text != "x" || !cols[0].Numeric {
				t.Errorf("simple: %+v %+v", cols, rows)
			}
			if c.batch == "" {
				return
			}
			cols, rows = exported(t, s, c.driver, c.batch, 0)
			if cols[0].Name != "name" || len(rows) != 2 {
				t.Errorf("first result: %+v %d rows", cols, len(rows))
			}
			cols, rows = exported(t, s, c.driver, c.batch, 1)
			if cols[0].Name != "seven" || len(rows) != 1 || rows[0][0].Text != "7" {
				t.Errorf("second result: %+v %+v", cols, rows)
			}
		})
	}
}
