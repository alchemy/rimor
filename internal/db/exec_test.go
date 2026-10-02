package db

import (
	"testing"
	"time"
)

func TestReturnsRows(t *testing.T) {
	for query, want := range map[string]bool{
		"SELECT 1":                              true,
		"  -- comment\n  select * from t":       true,
		"/* hi */ (SELECT 1) UNION SELECT 2":    true,
		"WITH x AS (SELECT 1) SELECT * FROM x":  true,
		"EXEC dbo.proc":                         true,
		"INSERT INTO t VALUES (1)":              false,
		"insert into t values (1) returning id": true,
		"UPDATE t SET a = 1 OUTPUT inserted.a":  true,
		"UPDATE t SET a = 1":                    false,
		"DELETE FROM t":                         false,
		"CREATE TABLE t (a int)":                false,
		"":                                      false,
	} {
		if got := returnsRows(query); got != want {
			t.Errorf("returnsRows(%q) = %v, want %v", query, got, want)
		}
	}
}

func TestFormatValue(t *testing.T) {
	ts := time.Date(2026, 10, 1, 14, 5, 0, 0, time.UTC)
	for _, c := range []struct {
		v    any
		typ  string
		want string
	}{
		{nil, "INT", "NULL"},
		{[]byte("text"), "VARCHAR", "text"},
		{[]byte{0xff, 0x00}, "VARBINARY", "0xFF00"},
		{ts, "DATE", "2026-10-01"},
		{ts, "DATETIME", "2026-10-01 14:05:00"},
		{2000.0, "FLOAT8", "2000"},
		{1e20, "FLOAT8", "1e+20"},
		{int64(42), "INT8", "42"},
		{true, "BOOL", "true"},
	} {
		if got := formatValue(c.v, c.typ).Text; got != c.want {
			t.Errorf("formatValue(%v, %s) = %q, want %q", c.v, c.typ, got, c.want)
		}
	}
}
