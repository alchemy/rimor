package ui

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
