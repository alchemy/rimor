package ui

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// agentDriver opens rimor on an SQLite "erp" connection, plus a
// read-only one, with plain icons.
func agentDriver(t *testing.T) *driver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "erp.db")
	c, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER REFERENCES customers(id), delivery_date TEXT)`,
		`INSERT INTO customers VALUES (1, 'Secret Customer SpA')`,
		`INSERT INTO orders VALUES (10, 1, '2020-01-01'), (11, 1, '2999-01-01')`,
	} {
		if _, err := c.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	store.Save([]db.Config{
		{Name: "ERP", Driver: db.SQLite, DSN: path},
		{Name: "archive", Driver: db.SQLite, DSN: path, ReadOnly: true},
	})
	settings := config.Defaults()
	settings.Icons = config.IconsPlain
	d := &driver{t: t, m: New(store, "", settings), wait: 5 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	// Close the file before the temporary directory goes: Windows cannot
	// delete a file that is open. Sessions go back to their pool, which
	// closes every connection.
	t.Cleanup(func() {
		for _, tb := range d.m.query.tabs {
			if tb.session != nil {
				tb.session.Close()
			}
		}
		d.m.explorer.Close()
	})
	return d
}

// call makes a tool call the way the socket does and returns the JSON the
// agent would get, or the error.
func (d *driver) call(tool, args string) (string, error) {
	d.t.Helper()
	reply := make(chan AgentReply, 1)
	d.send(AgentRequestMsg{Tool: tool, Args: json.RawMessage(args), Reply: reply})
	select {
	case r := <-reply:
		if r.Err != nil {
			return "", r.Err
		}
		out, err := json.Marshal(r.Result)
		if err != nil {
			d.t.Fatal(err)
		}
		return string(out), nil
	case <-time.After(10 * time.Second):
		d.t.Fatalf("%s: no reply", tool)
		return "", nil
	}
}

func (d *driver) mustCall(tool, args string) string {
	d.t.Helper()
	out, err := d.call(tool, args)
	if err != nil {
		d.t.Fatalf("%s %s: %v", tool, args, err)
	}
	return out
}

func requireIn(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in\n%s", w, got)
		}
	}
}

func TestAgentCatalog(t *testing.T) {
	d := agentDriver(t)

	got := d.mustCall("list_connections", "")
	if got != `{"connections":[{"name":"ERP","driver":"SQLite","read_only":false,"open":false},{"name":"archive","driver":"SQLite","read_only":true,"open":false}]}` {
		t.Errorf("list_connections: %s", got)
	}
	if strings.Contains(got, "erp.db") {
		t.Error("the file path reached the agent")
	}

	got = d.mustCall("list_objects", `{"connection":"erp"}`) // case ignored
	requireIn(t, got, `"tables":["customers","orders"]`, `"views":[]`)
	got = d.mustCall("list_objects", `{"connection":"ERP","kind":"view"}`)
	if got != `{"views":[]}` {
		t.Errorf("one kind: %s", got)
	}

	// Once open, the connection reports its version.
	requireIn(t, d.mustCall("list_connections", ""), `"open":true,"server_version":"SQLite 3.`)

	got = d.mustCall("search_objects", `{"connection":"ERP","text":"deliv"}`)
	if got != `{"matches":[{"kind":"column","name":"orders","column":"delivery_date","type":"TEXT"}]}` {
		t.Errorf("search: %s", got)
	}

	got = d.mustCall("describe", `{"connection":"ERP","name":"orders"}`)
	requireIn(t, got, `"kind":"table"`, `"name":"delivery_date"`,
		`"foreign_keys":[{"table":"orders","columns":["customer_id"],"ref_table":"customers","ref_columns":["id"]}]`)

	if _, err := d.call("describe", `{"connection":"ERP","name":"nope"}`); err == nil {
		t.Error("describe of a missing table succeeded")
	}
	_, err := d.call("list_objects", `{"connection":"CRM"}`)
	if err == nil || !strings.Contains(err.Error(), `the connections are "ERP", "archive"`) {
		t.Errorf("unknown connection: %v", err)
	}
	if _, err := d.call("list_objects", `{"connection":`); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestAgentOpensQueryTab(t *testing.T) {
	d := agentDriver(t)
	d.key("alt+1") // the user is in the explorer
	before := len(d.m.query.tabs)

	got := d.mustCall("open_query", `{"connection":"ERP","sql":"SELECT *\nFROM orders\nWHERE delivery_date < date('now')","title":"expired deliveries"}`)
	requireIn(t, got, `"title":"expired deliveries"`, `Not executed`)

	q := &d.m.query
	cur := q.current()
	if len(q.tabs) != before || !cur.agent || cur.title() != "expired deliveries" {
		t.Errorf("the empty tab was not replaced by the agent's: %d tabs, %+v", len(q.tabs), cur)
	}
	if cur.run != nil {
		t.Fatal("the agent's query ran")
	}
	if d.m.focus != focusQuery || cur.ctx.conn.cfg().Name != "ERP" {
		t.Errorf("focus %v, context %+v", d.m.focus, cur.ctx)
	}
	screen := d.screen()
	requireIn(t, screen, "✶ expired deliveries", "The agent wrote this query", "review it, then")

	// A second query gets a tab of its own, with a unique name.
	got = d.mustCall("open_query", `{"connection":"archive","sql":"DELETE FROM orders","title":"expired deliveries"}`)
	requireIn(t, got, `"title":"expired deliveries-2"`)
	if len(q.tabs) != before+1 {
		t.Errorf("%d tabs", len(q.tabs))
	}

	// The tab list shows both, and which is current.
	got = d.mustCall("list_tabs", "")
	requireIn(t, got, `"title":"expired deliveries","connection":"ERP","opened_by_agent":true`,
		`"connection":"archive","opened_by_agent":true,"current":true`)

	if _, err := d.call("open_query", `{"connection":"ERP","sql":"  "}`); err == nil {
		t.Error("empty SQL accepted")
	}
}

func TestAgentUpdatesOnlyItsUntouchedTabs(t *testing.T) {
	d := agentDriver(t)
	d.mustCall("open_query", `{"connection":"ERP","sql":"SELECT 1"}`)
	id := d.m.query.current().id

	args := func(sql string) string {
		b, _ := json.Marshal(map[string]any{"tab": id, "sql": sql})
		return string(b)
	}
	d.mustCall("update_query", args("SELECT 2"))
	if got := d.m.query.current().ed.Text(); got != "SELECT 2" {
		t.Errorf("text %q", got)
	}
	// The user can undo the revision.
	d.key("ctrl+z")
	if got := d.m.query.current().ed.Text(); got != "SELECT 1" {
		t.Errorf("after undo %q", got)
	}

	// The undo was the user's edit: the agent may no longer change the tab.
	if _, err := d.call("update_query", args("SELECT 3")); err == nil || !strings.Contains(err.Error(), "edited this tab") {
		t.Errorf("update after the user's edit: %v", err)
	}

	// Nor the user's own tabs.
	d.key("ctrl+t")
	b, _ := json.Marshal(map[string]any{"tab": d.m.query.current().id, "sql": "DROP TABLE orders"})
	if _, err := d.call("update_query", string(b)); err == nil || !strings.Contains(err.Error(), "the user's") {
		t.Errorf("update of a user tab: %v", err)
	}
	if _, err := d.call("update_query", `{"tab":999,"sql":"x"}`); err == nil {
		t.Error("update of a missing tab")
	}
}

func TestAgentReadsOutcomeNotRows(t *testing.T) {
	d := agentDriver(t)
	d.mustCall("open_query", `{"connection":"ERP","sql":"SELECT o.id, c.name FROM orders o JOIN customers c ON c.id = o.customer_id"}`)

	got := d.mustCall("read_tab", "")
	requireIn(t, got, `"sql":"SELECT o.id, c.name`, `"opened_by_agent":true`)
	if strings.Contains(got, "last_run") {
		t.Errorf("a run before the user ran it: %s", got)
	}

	d.key("ctrl+enter") // the user runs it
	d.settle()
	got = d.mustCall("read_tab", "")
	requireIn(t, got, `"status":"done"`, `"rows":2`, `"columns":[{"name":"id"`, `{"name":"name"`)
	if strings.Contains(got, "Secret Customer") {
		t.Errorf("result rows reached the agent: %s", got)
	}

	// A failure comes with the database's message.
	d.runSQL("SELECT nope FROM orders")
	got = d.mustCall("read_tab", "")
	requireIn(t, got, `"status":"error"`, `no such column: nope`)
}

func TestAgentTabSurvivesRestart(t *testing.T) {
	d := agentDriver(t)
	d.mustCall("open_query", `{"connection":"ERP","sql":"SELECT 1","title":"one"}`)
	s := d.m.query.snapshot()

	var q QueryPane
	q.restore(s, d.m.explorer.ConnectionByName)
	if len(q.tabs) != 1 || !q.tabs[0].agent || q.tabs[0].title() != "one" || q.tabs[0].ed.Text() != "SELECT 1" {
		t.Fatalf("restored %+v", q.tabs)
	}
	if q.tabs[0].ed.Version() != q.tabs[0].agentVersion {
		t.Error("the agent can no longer revise its restored tab")
	}
}

// request sends a tool call without running the command it returns: the
// timer of wait_for_run, which a test fires itself.
func (d *driver) request(tool, args string) <-chan AgentReply {
	reply := make(chan AgentReply, 1)
	next, _ := d.m.Update(AgentRequestMsg{Tool: tool, Args: json.RawMessage(args), Reply: reply})
	d.m = next.(Model)
	return reply
}

func received(t *testing.T, reply <-chan AgentReply) (string, error) {
	t.Helper()
	select {
	case r := <-reply:
		if r.Err != nil {
			return "", r.Err
		}
		out, _ := json.Marshal(r.Result)
		return string(out), nil
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
		return "", nil
	}
}

func pending(reply <-chan AgentReply) bool { return len(reply) == 0 }

const joinSQL = "SELECT o.id, c.name FROM orders o JOIN customers c ON c.id = o.customer_id ORDER BY o.id"

func TestAgentResultsNeedTheUsersGrant(t *testing.T) {
	d := agentDriver(t)
	got := d.mustCall("open_query", `{"connection":"ERP","sql":"`+joinSQL+`","want_results":true}`)
	requireIn(t, got, `"results":"requested`)
	requireIn(t, d.screen(), "✶ The agent wrote this query · F5 to run", "agent asks for results: alt+a")

	d.key("ctrl+enter")
	d.settle()
	if _, err := d.call("read_results", ""); err == nil || !strings.Contains(err.Error(), "press alt+a") {
		t.Fatalf("read without a grant: %v", err)
	}
	requireIn(t, d.mustCall("list_tabs", ""), `"results_requested":true`)

	// The user allows it.
	d.key("alt+a")
	requireIn(t, d.screen(), "✶ agent reads results", "Results shared with the agent · alt+a stops")
	got = d.mustCall("read_results", "")
	requireIn(t, got, `"rows":[["10","Secret Customer SpA"],["11","Secret Customer SpA"]]`, `"status":"done"`)
	if strings.Contains(got, "next_offset") {
		t.Errorf("a next page after the last row: %s", got)
	}
	got = d.mustCall("read_results", `{"offset":1,"limit":1}`)
	requireIn(t, got, `"offset":1,"rows":[["11","Secret Customer SpA"]]`)
	got = d.mustCall("read_results", `{"limit":1}`)
	requireIn(t, got, `"next_offset":1`)

	// Moving the tab to another connection ends the grant.
	id := d.m.query.current().id
	b, _ := json.Marshal(map[string]any{"tab": id, "sql": joinSQL, "connection": "archive"})
	d.mustCall("update_query", string(b))
	if _, err := d.call("read_results", ""); err == nil {
		t.Error("the grant survived a change of connection")
	}

	// And the key takes it back.
	d.key("alt+a")
	if !d.m.query.current().shareResults {
		t.Fatal("alt+a did not grant")
	}
	d.key("alt+a")
	if _, err := d.call("read_results", ""); err == nil {
		t.Error("alt+a did not revoke")
	}
}

func TestAgentResultPagesAreCapped(t *testing.T) {
	d := agentDriver(t)
	d.mustCall("open_query", `{"connection":"ERP","sql":"WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 1000) SELECT i, replace(hex(zeroblob(1500)), '0', 'é') AS big FROM c"}`)
	d.key("alt+a", "ctrl+enter")
	d.settle()

	var page struct {
		Page struct {
			Rows       [][]*string `json:"rows"`
			NextOffset *int        `json:"next_offset"`
			CutCells   string      `json:"cut_cells"`
		} `json:"page"`
	}
	json.Unmarshal([]byte(d.mustCall("read_results", `{"limit":500}`)), &page)
	n := len(page.Page.Rows)
	if n == 0 || n >= 100 || page.Page.NextOffset == nil || *page.Page.NextOffset != n {
		t.Fatalf("a page of 1 KB cells: %d rows, next %v", n, page.Page.NextOffset)
	}
	cell := *page.Page.Rows[0][1]
	if len(cell) > maxCellBytes+len("…") || !strings.HasSuffix(cell, "…") || page.Page.CutCells == "" {
		t.Errorf("cell of %d bytes, cut note %q", len(cell), page.Page.CutCells)
	}
	if !utf8.ValidString(cell) {
		t.Error("a character was cut in half")
	}

	d.runSQL("WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 1000) SELECT i, NULL AS empty FROM c")
	if r := d.m.query.current().run; r.err != nil || r.res == nil || r.res.Rows == nil {
		t.Fatalf("second run: err %+v, res %+v, text %q", r.err, r.res, d.m.query.current().ed.Text())
	}
	d.settle()
	var small, capped struct {
		Page struct {
			Rows [][]*string `json:"rows"`
		} `json:"page"`
	}
	json.Unmarshal([]byte(d.mustCall("read_results", "")), &small)
	if len(small.Page.Rows) != 100 || small.Page.Rows[0][1] != nil {
		t.Errorf("default page: %d rows, null as %v", len(small.Page.Rows), small.Page.Rows[0][1])
	}
	json.Unmarshal([]byte(d.mustCall("read_results", `{"limit":10000}`)), &capped)
	if len(capped.Page.Rows) != 500 {
		t.Errorf("limit above the cap: %d rows", len(capped.Page.Rows))
	}
}

func TestAgentWaitsForTheUsersRun(t *testing.T) {
	d := agentDriver(t)
	d.mustCall("open_query", `{"connection":"ERP","sql":"`+joinSQL+`"}`)

	reply := d.request("wait_for_run", "")
	if !pending(reply) {
		t.Fatal("answered before the user ran the tab")
	}
	d.key("ctrl+enter")
	d.settle()
	got, err := received(t, reply)
	if err != nil {
		t.Fatal(err)
	}
	requireIn(t, got, `"status":"done"`, `"rows":2`, `"results":"not shared`)
	if strings.Contains(got, "Secret") {
		t.Errorf("rows without a grant: %s", got)
	}

	// With the grant, the answer brings the first rows.
	d.key("alt+a")
	reply = d.request("wait_for_run", `{"timeout_seconds":5}`)
	d.key("ctrl+enter")
	d.settle()
	got, _ = received(t, reply)
	requireIn(t, got, `"page":{"offset":0,"rows":[["10","Secret Customer SpA"]`)

	// Nobody runs it: the timer ends the wait.
	reply = d.request("wait_for_run", "")
	d.send(waitTimeoutMsg{d.m.waiters[len(d.m.waiters)-1]})
	got, _ = received(t, reply)
	requireIn(t, got, `"status":"not run yet"`)
	if len(d.m.waiters) != 0 {
		t.Errorf("%d waiters left", len(d.m.waiters))
	}

	// The tab goes away.
	reply = d.request("wait_for_run", "")
	w := d.m.waiters[0]
	d.m.query.Close(d.m.query.current())
	d.send(waitTimeoutMsg{w})
	if _, err := received(t, reply); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("closed tab: %v", err)
	}
}

func TestAgentWarnsAboutWrites(t *testing.T) {
	d := agentDriver(t)
	got := d.mustCall("open_query", `{"connection":"ERP","sql":"-- tidy up\nDELETE FROM orders WHERE id = 10"}`)
	requireIn(t, got, `"warning":"The user was warned that this SQL may write (DELETE)."`)
	requireIn(t, d.screen(), "⚠ This query may write (DELETE) · review it, then F5 to run")

	// Words in comments and strings do not count.
	got = d.mustCall("open_query", `{"connection":"ERP","sql":"-- no DELETE here\nSELECT 'DROP' AS word"}`)
	if strings.Contains(got, "warning") {
		t.Errorf("warned about a SELECT: %s", got)
	}
}

func TestAgentErrorDetail(t *testing.T) {
	failed := func(d db.Driver, shared bool) map[string]any {
		tb := &tab{ctx: queryContext{conn: newConnection(db.Config{Driver: d})}, shareResults: shared,
			run: &run{err: &db.Error{Message: "failed", Detail: "Key (email)=(a@b.c) already exists."}}}
		return tb.runOutcome()["error"].(map[string]any)
	}
	if _, ok := failed(db.Postgres, false)["detail"]; ok {
		t.Error("a PostgreSQL detail, which can quote rows, reached the agent without a grant")
	}
	if _, ok := failed(db.Postgres, true)["detail"]; !ok {
		t.Error("no detail with the results shared")
	}
	if _, ok := failed(db.SQLServer, false)["detail"]; !ok {
		t.Error("no SQL Server detail: the batch's earlier messages explain the error")
	}
}
