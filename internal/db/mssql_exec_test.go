package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// mssqlSession is a session on the SQL Server test database. The tests
// only read, and write to temp tables.
func mssqlSession(t *testing.T) *sql.Conn {
	t.Helper()
	pool := NewPool(Config{Driver: SQLServer, DSN: serverDSN(t, "RIMOR_TEST_SQLSERVER")})
	t.Cleanup(pool.Close)
	s, err := pool.Session(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Release(s) })
	return s
}

// runMS runs a batch as the query pane does and waits for its rows.
func runMS(t *testing.T, conn *sql.Conn, batch string, limit int64) (*Result, Progress, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	res, err := Run(ctx, conn, batch, RunOptions{Driver: SQLServer, MemoryLimit: limit, Cancel: cancel})
	var p Progress
	if err == nil && res.Rows != nil {
		p = wait(t, res.Rows)
	}
	return res, p, err
}

// stillUsable checks the session answers after a batch.
func stillUsable(t *testing.T, conn *sql.Conn) {
	t.Helper()
	res, p, err := runMS(t, conn, "SELECT 42 AS answer", 0)
	if err != nil || res.Rows == nil || p.Rows != 1 || res.Rows.Cell(0, 0).Text != "42" {
		t.Fatalf("the session is not usable after the batch: %v %+v", err, p)
	}
}

func TestRunSQLServerBatches(t *testing.T) {
	conn := mssqlSession(t)

	// The case that showed "101 rows affected": a DECLARE before the SELECT.
	res, p, err := runMS(t, conn, `DECLARE @n int = 3;

SELECT TOP (@n) name, object_id
    FROM sys.objects
    ORDER BY object_id;`, 0)
	if err != nil || res.Rows == nil || p.Rows != 3 || p.Err != nil {
		t.Fatalf("DECLARE then SELECT: %v, %+v, %+v", err, res, p)
	}
	if cols := res.Rows.Columns(); len(cols) != 2 || cols[0].Name != "name" {
		t.Errorf("columns %+v", cols)
	}
	stillUsable(t, conn)

	// IF, and SET before the rows.
	res, p, err = runMS(t, conn, "SET NOCOUNT ON; IF 1 = 1 SELECT 'yes' AS answer ELSE SELECT 'no' AS answer", 0)
	if err != nil || res.Rows == nil || res.Rows.Cell(0, 0).Text != "yes" {
		t.Errorf("IF: %v %+v %+v", err, res, p)
	}

	// Only writes: the rows affected, added up. (SET NOCOUNT lasts for the
	// session, and ON sends no counts.)
	res, _, err = runMS(t, conn, `SET NOCOUNT OFF;
CREATE TABLE #t (a int);
INSERT INTO #t VALUES (1), (2), (3);
UPDATE #t SET a = a + 1 WHERE a > 1;`, 0)
	if err != nil || res.Rows != nil || !res.HasAffected || res.RowsAffected != 5 {
		t.Errorf("writes: %v %+v", err, res)
	}
	// The temp table outlives the batch: the session is the tab's.
	res, p, _ = runMS(t, conn, "SELECT a FROM #t ORDER BY a", 0)
	if res == nil || res.Rows == nil || p.Rows != 3 {
		t.Errorf("temp table: %+v", p)
	}

	// PRINT before the rows is kept.
	res, _, err = runMS(t, conn, "PRINT 'checking'; SELECT 1 AS one", 0)
	if err != nil || len(res.Messages) != 1 || res.Messages[0] != "checking" {
		t.Errorf("messages: %v %+v", err, res)
	}

	// Two result sets: the first is shown, the second read past.
	res, p, err = runMS(t, conn, "SELECT 1 AS first; SELECT 2 AS second", 0)
	if err != nil || res.Rows.Columns()[0].Name != "first" || p.Err != nil {
		t.Errorf("two sets: %v %+v", err, p)
	}
	stillUsable(t, conn)
}

func TestRunSQLServerErrors(t *testing.T) {
	conn := mssqlSession(t)

	// Before any rows: the batch fails.
	_, _, err := runMS(t, conn, "DECLARE @x int = 1; SELECT * FROM rimor_no_such_table", 0)
	if err == nil || !strings.Contains(err.Error(), "rimor_no_such_table") {
		t.Errorf("missing table: %v", err)
	}
	stillUsable(t, conn)

	// During the rows: the error is the fetch's.
	res, p, err := runMS(t, conn, "SELECT 1 / 0 AS boom", 0)
	if err == nil && (p.Err == nil || !strings.Contains(p.Err.Error(), "zero")) {
		t.Errorf("divide by zero: %v %+v %+v", err, res, p)
	}
	stillUsable(t, conn)

	// After the rows: reported too, with the rows kept.
	res, p, err = runMS(t, conn, "SELECT 1 AS fine; SELECT * FROM rimor_no_such_table", 0)
	if err != nil || p.Rows != 1 || p.Err == nil || !strings.Contains(p.Err.Error(), "rimor_no_such_table") {
		t.Errorf("error after the rows: %v %+v", err, p)
	}
	stillUsable(t, conn)
}

func TestRunSQLServerStops(t *testing.T) {
	conn := mssqlSession(t)
	// Far more rows than a 64 KB limit takes, then more of the batch.
	_, p, err := runMS(t, conn, `DECLARE @n int = 200000;
WITH c AS (SELECT TOP (@n) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
           FROM sys.all_objects a CROSS JOIN sys.all_objects b)
SELECT i, REPLICATE('x', 50) AS pad FROM c;
SELECT 'after' AS later;`, 64<<10)
	if err != nil || p.Stopped != StopLimit || p.Rows == 0 {
		t.Fatalf("limit: %v %+v", err, p)
	}
	stillUsable(t, conn)
}
