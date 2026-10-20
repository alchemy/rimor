package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
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

// runMS runs a batch as the query pane does and waits for all of it; the
// progress is the first result set's.
func runMS(t *testing.T, conn *sql.Conn, batch string, limit int64) (*Result, Progress, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	res, err := Run(ctx, conn, batch, RunOptions{Driver: SQLServer, MemoryLimit: limit, Cancel: cancel})
	var p Progress
	if res != nil && res.Batch != nil {
		waitBatch(t, res.Batch)
	}
	if err == nil && res.Rows != nil {
		p = wait(t, res.Rows)
	}
	return res, p, err
}

func waitBatch(t *testing.T, b *Batch) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for !b.Done() {
		select {
		case <-b.Updated():
		case <-deadline:
			t.Fatal("the batch did not end")
		}
	}
}

// log writes a batch's messages compactly: "rows 3", "info hi", "error …".
func logOf(b *Batch) []string {
	msgs, _ := b.Messages()
	var out []string
	for _, m := range msgs {
		switch m.Kind {
		case MessageRows:
			out = append(out, fmt.Sprintf("%d: rows %d", m.Result, m.Count))
		case MessageInfo:
			out = append(out, fmt.Sprintf("%d: info %s", m.Result, m.Text))
		case MessageError:
			out = append(out, fmt.Sprintf("%d: error %s", m.Result, firstLineOf(m.Text)))
		}
	}
	return out
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
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
	if got := logOf(res.Batch); err != nil || len(got) < 1 || got[0] != "0: info checking" {
		t.Errorf("messages: %v %q", err, got)
	}

	// Two result sets, both kept.
	res, p, err = runMS(t, conn, "SELECT 1 AS first; SELECT 2 AS second", 0)
	if err != nil || res.Rows.Columns()[0].Name != "first" || p.Err != nil {
		t.Errorf("two sets: %v %+v", err, p)
	}
	if sets := res.Sets(); len(sets) != 2 || sets[1].Columns()[0].Name != "second" || sets[1].Cell(0, 0).Text != "2" {
		t.Errorf("second set: %d sets", len(sets))
	}
	stillUsable(t, conn)
}

func TestRunSQLServerResultsAndMessages(t *testing.T) {
	conn := mssqlSession(t)
	res, _, err := runMS(t, conn, `SET NOCOUNT OFF;
PRINT 'start';
CREATE TABLE #m (a int);
INSERT INTO #m VALUES (1), (2);
SELECT a FROM #m ORDER BY a;
PRINT 'between';
SELECT COUNT(*) AS n FROM #m;
RAISERROR ('low severity', 5, 1);
SELECT 'last' AS word;`, 0)
	if err != nil {
		t.Fatal(err)
	}
	sets := res.Sets()
	if len(sets) != 3 || sets[0].Len() != 2 || sets[1].Cell(0, 0).Text != "2" || sets[2].Cell(0, 0).Text != "last" {
		t.Fatalf("%d sets", len(sets))
	}
	want := []string{"0: info start", "0: rows 2", "1: rows 2", "1: info between", "2: rows 1", "2: info low severity", "3: rows 1"}
	if got := logOf(res.Batch); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("log:\n%q\nwant:\n%q", got, want)
	}
	if res.Batch.Err() != nil || res.Batch.Stopped() != StopNone {
		t.Errorf("err %v, stopped %v", res.Batch.Err(), res.Batch.Stopped())
	}

	// An error between results: logged in place, the results around it kept.
	res, _, err = runMS(t, conn, "SELECT 1 AS before; SELECT 1 / 0 AS boom; SELECT 3 AS after;", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(logOf(res.Batch), "|")
	if !strings.Contains(got, "error Msg 8134, Level 16, State 1, Line 1") || res.Batch.Err() == nil {
		t.Errorf("log %q", got)
	}
	if sets := res.Sets(); len(sets) != 3 || sets[2].Cell(0, 0).Text != "3" {
		t.Errorf("results around the error: %d", len(sets))
	}
	stillUsable(t, conn)

	// Only messages: no rows, the log has them.
	res, _, err = runMS(t, conn, "PRINT 'only words'", 0)
	if err != nil || res.Rows != nil || strings.Join(logOf(res.Batch), "|") != "0: info only words" {
		t.Errorf("messages only: %v %q", err, logOf(res.Batch))
	}

	// A flood of messages is capped.
	res, _, err = runMS(t, conn, "DECLARE @i int = 0; WHILE @i < 10010 BEGIN PRINT @i; SET @i += 1; END", 0)
	if msgs, dropped := res.Batch.Messages(); err != nil || len(msgs) != maxMessages || dropped < 10 {
		t.Errorf("flood: %v, %d kept, %d dropped", err, len(msgs), dropped)
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

	// During the rows: the batch's error, after a result without rows.
	res, p, err := runMS(t, conn, "SELECT 1 / 0 AS boom", 0)
	if err != nil || p.Rows != 0 || res.Batch.Err() == nil || !strings.Contains(res.Batch.Err().Error(), "zero") {
		t.Errorf("divide by zero: %v %+v %v", err, p, res.Batch.Err())
	}
	stillUsable(t, conn)

	// After the rows: the batch's error, with the rows kept.
	res, p, err = runMS(t, conn, "SELECT 1 AS fine; SELECT * FROM rimor_no_such_table", 0)
	if err != nil || p.Rows != 1 || res.Batch.Err() == nil || !strings.Contains(res.Batch.Err().Error(), "rimor_no_such_table") {
		t.Errorf("error after the rows: %v %+v %v", err, p, res.Batch.Err())
	}
	stillUsable(t, conn)
}

func TestRunSQLServerStops(t *testing.T) {
	conn := mssqlSession(t)
	// Far more rows than a 64 KB limit takes, then more of the batch.
	limited, p, err := runMS(t, conn, `DECLARE @n int = 200000;
WITH c AS (SELECT TOP (@n) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
           FROM sys.all_objects a CROSS JOIN sys.all_objects b)
SELECT i, REPLICATE('x', 50) AS pad FROM c;
SELECT 'after' AS later;`, 64<<10)
	if err != nil || p.Stopped != StopLimit || p.Rows == 0 {
		t.Fatalf("limit: %v %+v", err, p)
	}
	if log := strings.Join(logOf(limited.Batch), "|"); limited.Batch.Stopped() != StopLimit || len(limited.Sets()) != 1 ||
		!strings.Contains(log, "memory limit") {
		t.Errorf("the batch does not say it stopped: %v, %q", limited.Batch.Stopped(), log)
	}
	stillUsable(t, conn)

	// Stopping by hand keeps what arrived and frees the session.
	ctx, cancel := context.WithCancel(context.Background())
	big, err := Run(ctx, conn, `WITH c AS (SELECT TOP (2000000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
           FROM sys.all_objects a CROSS JOIN sys.all_objects b)
SELECT i FROM c; SELECT 'never' AS later;`, RunOptions{Driver: SQLServer, MemoryLimit: 1 << 30, Cancel: cancel})
	if err != nil {
		t.Fatal(err)
	}
	for big.Rows.Len() == 0 {
		<-big.Updated()
	}
	big.Stop()
	waitBatch(t, big.Batch)
	if big.Batch.Stopped() != StopCancelled || len(big.Sets()) != 1 {
		t.Errorf("stop: %v, %d sets", big.Batch.Stopped(), len(big.Sets()))
	}
	stillUsable(t, conn)
}
