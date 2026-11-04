package ui

import (
	"context"
	"strings"
	"testing"

	"rimor.dev/internal/db"
)

func TestChangeTab(t *testing.T) {
	d, raw := txDriver(t)
	if _, err := raw.Exec("INSERT INTO t VALUES (1), (2); CREATE VIEW v AS SELECT a FROM t WHERE a > 1"); err != nil {
		t.Fatal(err)
	}
	cur := d.m.query.current()
	scripts, err := cur.ctx.conn.pool.Scripts(context.Background(), db.Object{Kind: db.KindView, Name: "v"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var change db.Script
	for _, s := range scripts {
		if s.Change != nil {
			change = s
		}
	}
	d.send(openScriptMsg{cur.ctx, "v", change})
	tab := d.m.query.current()
	if tab.change == nil || !strings.Contains(d.queryStatus(), "changes view v") {
		t.Fatalf("not a change tab: %q", d.queryStatus())
	}
	if f := stripHints(d.m.query.Footer(true)); !strings.Contains(f, "running the whole tab changes it") {
		t.Errorf("notice %q", f)
	}
	d.m.query.notice = ""
	if f := stripHints(d.m.query.Footer(true)); !strings.Contains(f, "change the view") {
		t.Errorf("footer %q", f)
	}

	// Running the whole tab changes the view, and says so.
	tab.ed.SetText(strings.Replace(tab.ed.Text(), "a > 1", "a > 0", 1))
	d.key("ctrl+enter")
	var n int
	raw.QueryRow("SELECT count(*) FROM v").Scan(&n)
	if n != 2 || !strings.Contains(stripHints(d.m.query.Footer(true)), "✓ Changed the view v") {
		t.Fatalf("%d rows; footer %q; run %+v", n, stripHints(d.m.query.Footer(true)), tab.run.err)
	}
	// Again: the tab compares with what it made, not with what it opened.
	d.key("ctrl+enter")
	if tab.run.err != nil || d.m.modal != nil {
		t.Fatalf("the tab's own change counted as someone else's: %+v", tab.run.err)
	}

	// Someone else changes it: asked before overwriting.
	raw.Exec("DROP VIEW v; CREATE VIEW v AS SELECT 7 AS a")
	d.key("ctrl+enter")
	if _, ok := d.m.modal.(*changeDialog); !ok {
		t.Fatalf("no dialog:\n%s", d.screen())
	}
	if !strings.Contains(d.screen(), "changed by someone else") {
		t.Errorf("dialog:\n%s", d.screen())
	}
	if place, _ := d.m.hereScopes(); place != "Changed meanwhile" {
		t.Errorf("help place %q", place)
	}
	d.key("esc")
	raw.QueryRow("SELECT count(*) FROM v").Scan(&n)
	if n != 1 {
		t.Fatal("cancel applied the change")
	}
	d.key("ctrl+enter", "o")
	raw.QueryRow("SELECT count(*) FROM v").Scan(&n)
	if n != 2 || d.m.modal != nil {
		t.Errorf("overwrite: %d rows", n)
	}

	// Other SQL in its place is not taken for the view.
	d.runSQL("SELECT 1")
	raw.QueryRow("SELECT count(*) FROM v").Scan(&n)
	if tab.run.err == nil || n != 2 {
		t.Errorf("SELECT 1 replaced the view: %d rows, %+v", n, tab.run.err)
	}

	// A change of context forgets the object.
	tab.setContext(queryContext{})
	if tab.change != nil {
		t.Error("the change outlived its context")
	}
}

func TestChangeTabSession(t *testing.T) {
	d, raw := txDriver(t)
	raw.Exec("CREATE VIEW v AS SELECT 1 AS a")
	cur := d.m.query.current()
	scripts, _ := cur.ctx.conn.pool.Scripts(context.Background(), db.Object{Kind: db.KindView, Name: "v"}, 10)
	for _, s := range scripts {
		if s.Change != nil {
			d.send(openScriptMsg{cur.ctx, "v", s})
		}
	}
	snap := d.m.query.snapshot()
	st := snap.Tabs[len(snap.Tabs)-1]
	if st.Change == nil || st.Content == nil || !st.Opened {
		t.Fatalf("session tab %+v", st)
	}

	var q QueryPane
	q.restore(snap, func(string) *connection { return cur.ctx.conn })
	back := q.tabs[len(q.tabs)-1]
	if back.change == nil || back.ed.Text() != d.m.query.current().ed.Text() || back.dirty() {
		t.Errorf("restored: change %v, dirty %v", back.change, back.dirty())
	}
}
