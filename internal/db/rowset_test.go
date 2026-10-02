package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// counting is a query returning n rows: an integer, a text and a value
// that is NULL on every tenth row.
func counting(n int) string {
	return fmt.Sprintf(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < %d)
		SELECT i, 'row ' || i AS label, CASE WHEN i %% 10 = 0 THEN NULL ELSE i * 1.5 END AS amount FROM c`, n)
}

func sqliteSession(t *testing.T) *sql.Conn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rows.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.Exec("CREATE TABLE x (a int)") // creates the file
	raw.Close()
	pool := NewPool(Config{Driver: SQLite, DSN: path})
	t.Cleanup(pool.Close)
	s, err := pool.Session(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runAll(t *testing.T, conn *sql.Conn, query string, limit int64) (*RowSet, Progress) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	res, err := Run(ctx, conn, query, RunOptions{MemoryLimit: limit, Cancel: cancel})
	if err != nil || res.Rows == nil {
		t.Fatalf("run: %v %+v", err, res)
	}
	return res.Rows, wait(t, res.Rows)
}

func wait(t *testing.T, set *RowSet) Progress {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		if p := set.Progress(); p.Done {
			return p
		}
		select {
		case <-set.Updated():
		case <-deadline:
			t.Fatal("fetch did not finish")
		}
	}
}

func TestRowSetFetchesEverything(t *testing.T) {
	conn := sqliteSession(t)
	set, p := runAll(t, conn, counting(100_000), 0)
	if p.Rows != 100_000 || p.Stopped != StopNone || p.Err != nil {
		t.Fatalf("progress = %+v", p)
	}
	for _, c := range []struct {
		row, col int
		want     Value
	}{
		{0, 0, Value{Text: "1"}},
		{0, 1, Value{Text: "row 1"}},
		{0, 2, Value{Text: "1.5"}},
		{9, 2, Value{Text: "NULL", Null: true}},
		{99_999, 0, Value{Text: "100000"}},
		{54_321, 1, Value{Text: "row 54322"}},
	} {
		if got := set.Cell(c.row, c.col); got != c.want {
			t.Errorf("cell %d,%d = %+v, want %+v", c.row, c.col, got, c.want)
		}
	}
	if !set.Numeric(0) || set.Numeric(1) || !set.Numeric(2) {
		t.Errorf("numeric = %v %v %v", set.Numeric(0), set.Numeric(1), set.Numeric(2))
	}
	// Compact storage: well under a string per cell (~40 bytes each).
	if perCell := float64(p.Bytes) / (100_000 * 3); perCell > 16 {
		t.Errorf("%.1f bytes per cell", perCell)
	}

	// The session is free again: another statement runs on it.
	if _, p := runAll(t, conn, "SELECT 1", 0); p.Rows != 1 {
		t.Errorf("second run = %+v", p)
	}
}

func TestRowSetStopsAtMemoryLimit(t *testing.T) {
	conn := sqliteSession(t)
	_, p := runAll(t, conn, counting(1_000_000), 1<<20)
	if p.Stopped != StopLimit || p.Rows == 0 || p.Rows >= 1_000_000 {
		t.Fatalf("progress = %+v", p)
	}
	if p.Bytes < 1<<20 || p.Bytes > 2<<20 {
		t.Errorf("stopped at %d bytes", p.Bytes)
	}
}

func TestRowSetStop(t *testing.T) {
	conn := sqliteSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	res, err := Run(ctx, conn, counting(50_000_000), RunOptions{Cancel: cancel})
	if err != nil {
		t.Fatal(err)
	}
	<-res.Rows.Updated() // some rows arrived
	res.Rows.Stop()
	p := wait(t, res.Rows)
	if p.Stopped != StopCancelled || p.Err != nil || p.Rows == 0 || p.Rows >= 50_000_000 {
		t.Fatalf("progress = %+v", p)
	}
	if _, p := runAll(t, conn, "SELECT 2", 0); p.Rows != 1 {
		t.Errorf("session unusable after stop: %+v", p)
	}
}

func TestRunWithoutRows(t *testing.T) {
	conn := sqliteSession(t)
	cancelled := false
	res, err := Run(context.Background(), conn, "INSERT INTO x VALUES (1), (2)", RunOptions{Cancel: func() { cancelled = true }})
	if err != nil || res.HasRows() || !res.HasAffected || res.RowsAffected != 2 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if !cancelled {
		t.Error("context not released")
	}
}
