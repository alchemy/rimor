package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// released checks that releasing a session closes its connection: the
// pool has one connection fewer, and none more idle.
func released(t *testing.T, p *Pool) {
	t.Helper()
	ctx := context.Background()
	conn, err := p.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.Session(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	before := conn.Stats()
	p.Release(s)
	after := conn.Stats()
	if after.OpenConnections != before.OpenConnections-1 || after.Idle != before.Idle {
		t.Errorf("released session went back to the pool: open %d → %d, idle %d → %d",
			before.OpenConnections, after.OpenConnections, before.Idle, after.Idle)
	}
	if _, err := s.ExecContext(ctx, "SELECT 1"); err == nil {
		t.Error("a released session still runs statements")
	}
}

func TestReleaseClosesTheConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	raw, _ := sql.Open("sqlite", path)
	raw.Exec("CREATE TABLE t (a int)")
	raw.Close()
	lite := NewPool(Config{Driver: SQLite, DSN: path})
	defer lite.Close()
	released(t, lite)

	for _, c := range []struct {
		env    string
		driver Driver
	}{{"RIMOR_TEST_POSTGRES", Postgres}, {"RIMOR_TEST_SQLSERVER", SQLServer}} {
		t.Run(string(c.driver), func(t *testing.T) {
			p := NewPool(Config{Driver: c.driver, DSN: serverDSN(t, c.env)})
			defer p.Close()
			released(t, p)
		})
	}
}

// An open transaction ends with its tab: it must never reach the pool's
// next user, which on SQLite saw the tab's uncommitted rows and could
// commit them.
func TestReleaseEndsTheTransactionSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tx.db")
	raw, _ := sql.Open("sqlite", path)
	raw.Exec("CREATE TABLE t (a int)")
	raw.Close()
	p := NewPool(Config{Driver: SQLite, DSN: path})
	defer p.Close()

	s, err := p.Session(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"BEGIN", "INSERT INTO t VALUES (1)"} {
		if _, err := s.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	p.Release(s) // as closing the tab does

	conn, _ := p.DB(ctx, "")
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 0 {
		t.Errorf("the pool sees %d uncommitted rows (%v)", n, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err == nil {
		t.Error("COMMIT from the pool committed the closed tab's transaction")
	}
}

// Closing rolls back at once, so the tab's locks go with it.
func TestReleaseEndsTheTransactionPostgres(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_POSTGRES")
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{"DROP TABLE IF EXISTS rimor_release", "CREATE TABLE rimor_release (a int)", "INSERT INTO rimor_release VALUES (1)"} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	defer admin.Exec("DROP TABLE rimor_release")

	p := NewPool(Config{Driver: Postgres, DSN: dsn})
	defer p.Close()
	s, err := p.Session(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"BEGIN", "UPDATE rimor_release SET a = 2"} {
		if _, err := s.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	p.Release(s)

	c, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.ExecContext(ctx, "SET lock_timeout = '3s'")
	start := time.Now()
	if _, err := c.ExecContext(ctx, "UPDATE rimor_release SET a = a + 10"); err != nil {
		t.Fatalf("the row is still locked by the closed tab: %v", err)
	}
	var a int
	c.QueryRowContext(ctx, "SELECT a FROM rimor_release").Scan(&a)
	if a != 11 || time.Since(start) > time.Second {
		t.Errorf("a = %d after %v: the tab's update was not rolled back at once", a, time.Since(start))
	}
}
