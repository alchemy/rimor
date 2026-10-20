package ui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/db"
)

// runBatch runs SQL in the current tab and waits for the whole batch.
func (d *driver) runBatch(sql string) {
	d.t.Helper()
	d.runSQL(sql)
	r := d.m.query.current().run
	deadline := time.Now().Add(30 * time.Second)
	for r.res != nil && r.res.Fetching() {
		if time.Now().After(deadline) {
			d.t.Fatal("the batch did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.send(rowsMsg{d.m.query.current(), r.gen})
}

func (d *driver) resultsStatus() string {
	return ansi.Strip(d.m.results.Status(d.m.query.current(), ""))
}

func TestResultsSQLServerBatch(t *testing.T) {
	dsn := os.Getenv("RIMOR_TEST_SQLSERVER")
	if dsn == "" {
		t.Skip("RIMOR_TEST_SQLSERVER not set")
	}
	d := queryDriver(t, db.Config{Name: "erp", Driver: db.SQLServer, DSN: dsn}, 10*time.Second)

	d.runBatch(`SET NOCOUNT OFF;
PRINT 'start';
SELECT 1 AS a UNION ALL SELECT 2;
SELECT 'x' AS b;
SELECT 1 / 0 AS boom;`)
	if st := d.resultsStatus(); !strings.HasPrefix(st, "[1/3] ✓ 2 rows") || !strings.Contains(st, "✗ error (m)") {
		t.Errorf("status %q", st)
	}
	d.key("alt+3")
	if f := ansi.Strip(d.m.results.Footer(d.m.query.current(), true)); !strings.Contains(f, "[ ] result") || !strings.Contains(f, "m messages") {
		t.Errorf("footer %q", f)
	}

	// The second result, then round to the first again.
	d.key("]")
	if s := d.results(); !strings.Contains(s, " b ") && !strings.Contains(s, "b\n") {
		t.Logf("second result:\n%s", s)
	}
	if st := d.resultsStatus(); !strings.HasPrefix(st, "[2/3] ✓ 1 row") {
		t.Errorf("status %q", st)
	}
	// Only the first result of a batch can be edited.
	d.key("enter")
	if p, ok := d.m.modal.(*cellPopup); !ok || !strings.Contains(p.reason, "first result") {
		t.Errorf("edit of result 2: %+v", d.m.modal)
	}
	d.key("esc", "]", "]")
	if st := d.resultsStatus(); !strings.HasPrefix(st, "[1/3]") {
		t.Errorf("wrap: %q", st)
	}

	// The messages, in order.
	d.key("m")
	s := d.results()
	for _, w := range []string{"start", "(2 rows affected)", "── after result 1", "(1 row affected)", "Msg 8134, Level 16", "Divide by zero"} {
		if !strings.Contains(s, w) {
			t.Errorf("messages lack %q:\n%s", w, s)
		}
	}
	if place, _ := d.m.hereScopes(); place != "Messages" {
		t.Errorf("help place %q", place)
	}
	d.key("esc")
	if d.m.query.current().run.messages {
		t.Error("esc did not leave the messages")
	}

	// Nothing but an error: the error view, and the messages one key away.
	d.runBatch("PRINT 'about to fail'; SELECT * FROM rimor_no_such_table;")
	s = d.results()
	if !strings.Contains(s, "Query failed") || !strings.Contains(s, "rimor_no_such_table") {
		t.Errorf("error view:\n%s", s)
	}
	d.key("m")
	if s := d.results(); !strings.Contains(s, "about to fail") {
		t.Errorf("messages after an error:\n%s", s)
	}

	// Only messages: they say there are some.
	d.runBatch("PRINT 'hello'")
	if s := d.results(); !strings.Contains(s, "Statement executed") || !strings.Contains(s, "m messages") {
		t.Errorf("messages only:\n%s", s)
	}
}

func TestAgentSeesBatches(t *testing.T) {
	dsn := os.Getenv("RIMOR_TEST_SQLSERVER")
	if dsn == "" {
		t.Skip("RIMOR_TEST_SQLSERVER not set")
	}
	d := queryDriver(t, db.Config{Name: "erp", Driver: db.SQLServer, DSN: dsn}, 10*time.Second)
	d.runBatch("PRINT 'secret note'; SELECT 1 AS a; SELECT 'two' AS b UNION ALL SELECT 'three';")

	got := d.mustCall("read_tab", "")
	requireIn(t, got, `"results":[{"columns":[{"name":"a"`, `"result":2,"rows":2`, `"messages":["(1 rows affected)","(2 rows affected)"]`,
		`"messages_withheld":"1 PRINT or info messages`)
	// The SQL says "secret note" too; the messages must not.
	if strings.Contains(got, `"secret note"`) || strings.Count(got, "secret note") != 2 {
		t.Errorf("PRINT reached the agent without a grant: %s", got)
	}

	d.key("alt+a")
	got = d.mustCall("read_tab", "")
	requireIn(t, got, `"secret note"`)
	got = d.mustCall("read_results", `{"result":2}`)
	requireIn(t, got, `"page":{"offset":0,"rows":[["two"],["three"]]}`, `"page_result":2`)
	if _, err := d.call("read_results", `{"result":5}`); err == nil {
		t.Error("result 5 of 2")
	}
}
