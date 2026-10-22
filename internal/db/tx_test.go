package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// txSession is a session of the given engine, with a table t (a int)
// holding one row: a temp table on the servers, which the tests may write.
func txSession(t *testing.T, d Driver) *sql.Conn {
	t.Helper()
	ctx := context.Background()
	var cfg Config
	switch d {
	case SQLite:
		path := filepath.Join(t.TempDir(), "tx.db")
		raw, _ := sql.Open("sqlite", path)
		raw.Exec("CREATE TABLE t (a int); INSERT INTO t VALUES (1)")
		raw.Close()
		cfg = Config{Driver: SQLite, DSN: path}
	case Postgres:
		cfg = Config{Driver: Postgres, DSN: serverDSN(t, "RIMOR_TEST_POSTGRES")}
	case SQLServer:
		cfg = Config{Driver: SQLServer, DSN: serverDSN(t, "RIMOR_TEST_SQLSERVER")}
	}
	p := NewPool(cfg)
	t.Cleanup(p.Close)
	s, err := p.Session(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Release(s) })
	switch d {
	case Postgres:
		mustExec(t, s, "CREATE TEMP TABLE t (a int)", "INSERT INTO t VALUES (1)")
	case SQLServer:
		mustExec(t, s, "SET NOCOUNT ON; CREATE TABLE #t (a int); INSERT INTO #t VALUES (1)")
	}
	return s
}

func txIs(t *testing.T, s *sql.Conn, d Driver, step string, want TxState) {
	t.Helper()
	got, err := Transaction(context.Background(), s, d)
	if err != nil || got != want {
		t.Errorf("%s: %+v (%v), want %+v", step, got, err, want)
	}
}

func value(t *testing.T, s *sql.Conn, table string) int {
	t.Helper()
	var a int
	if err := s.QueryRowContext(context.Background(), "SELECT a FROM "+table).Scan(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

var (
	none   = TxState{}
	open   = TxState{Open: true, Levels: 1}
	failed = TxState{Open: true, Failed: true, Levels: 1}
)

// Commit, roll back, and the states in between, the same on every engine.
func testTransactions(t *testing.T, d Driver) {
	ctx := context.Background()
	s := txSession(t, d)
	table, begin := "t", "BEGIN"
	if d == SQLServer {
		table, begin = "#t", "BEGIN TRANSACTION"
	}

	txIs(t, s, d, "fresh session", none)
	txIs(t, s, d, "asked twice: asking leaves nothing open", none)

	mustExec(t, s, begin, "UPDATE "+table+" SET a = 2")
	txIs(t, s, d, "BEGIN and an UPDATE", open)
	if v := value(t, s, table); v != 2 {
		t.Errorf("asking undid the pending update: a = %d", v)
	}
	if err := EndTransaction(ctx, s, d, false); err != nil {
		t.Fatal(err)
	}
	txIs(t, s, d, "rolled back", none)
	if v := value(t, s, table); v != 1 {
		t.Errorf("after rollback a = %d", v)
	}

	mustExec(t, s, begin, "UPDATE "+table+" SET a = 3")
	if err := EndTransaction(ctx, s, d, true); err != nil {
		t.Fatal(err)
	}
	txIs(t, s, d, "committed", none)
	if v := value(t, s, table); v != 3 {
		t.Errorf("after commit a = %d", v)
	}
}

func TestTransactionsSQLite(t *testing.T) {
	testTransactions(t, SQLite)

	s := txSession(t, SQLite)
	// A SAVEPOINT outside BEGIN opens a transaction too, which COMMIT ends.
	mustExec(t, s, "SAVEPOINT sp", "UPDATE t SET a = 5")
	txIs(t, s, SQLite, "SAVEPOINT", open)
	if err := EndTransaction(context.Background(), s, SQLite, true); err != nil {
		t.Fatal(err)
	}
	txIs(t, s, SQLite, "SAVEPOINT committed", none)
	if v := value(t, s, "t"); v != 5 {
		t.Errorf("a = %d", v)
	}
	// An error inside the transaction leaves it open.
	mustExec(t, s, "BEGIN")
	s.ExecContext(context.Background(), "SELECT * FROM nope")
	txIs(t, s, SQLite, "error inside", open)
}

func TestTransactionsPostgres(t *testing.T) {
	testTransactions(t, Postgres)

	// After an error the transaction can only be rolled back; a COMMIT
	// would quietly roll back, so committing is refused.
	ctx := context.Background()
	s := txSession(t, Postgres)
	mustExec(t, s, "BEGIN", "UPDATE t SET a = 9")
	s.ExecContext(ctx, "SELECT * FROM nope")
	txIs(t, s, Postgres, "error inside", failed)
	if err := EndTransaction(ctx, s, Postgres, true); !errors.Is(err, ErrTxFailed) {
		t.Errorf("commit of a failed transaction: %v", err)
	}
	if err := EndTransaction(ctx, s, Postgres, false); err != nil {
		t.Fatal(err)
	}
	txIs(t, s, Postgres, "rolled back", none)
}

func TestTransactionsSQLServer(t *testing.T) {
	testTransactions(t, SQLServer)

	ctx := context.Background()
	s := txSession(t, SQLServer)
	// Nested levels: committing ends them all.
	mustExec(t, s, "BEGIN TRANSACTION", "BEGIN TRANSACTION", "UPDATE #t SET a = 7")
	txIs(t, s, SQLServer, "two levels", TxState{Open: true, Levels: 2})
	if err := EndTransaction(ctx, s, SQLServer, true); err != nil {
		t.Fatal(err)
	}
	txIs(t, s, SQLServer, "all levels committed", none)

	// IMPLICIT_TRANSACTIONS opens one without BEGIN.
	mustExec(t, s, "SET IMPLICIT_TRANSACTIONS ON", "UPDATE #t SET a = 8")
	txIs(t, s, SQLServer, "implicit transaction", open)
	if err := EndTransaction(ctx, s, SQLServer, false); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "SET IMPLICIT_TRANSACTIONS OFF")
	txIs(t, s, SQLServer, "implicit rolled back", none)
	if v := value(t, s, "#t"); v != 7 {
		t.Errorf("a = %d", v)
	}
}
