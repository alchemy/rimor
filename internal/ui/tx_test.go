package ui

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// txDriver opens rimor on two SQLite connections to one file, with a tab
// on the first, and a separate connection to look from outside.
func txDriver(t *testing.T) (*driver, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tx.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE t (a int)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	store.Save([]db.Config{{Name: "shop", Driver: db.SQLite, DSN: path}, {Name: "other", Driver: db.SQLite, DSN: path}})
	d := &driver{t: t, m: New(store, "", config.Defaults()), wait: 5 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	d.key("down", "ctrl+t")
	t.Cleanup(func() {
		for _, tb := range d.m.query.tabs {
			if tb.session != nil {
				db.CloseSession(tb.session)
			}
		}
		d.m.explorer.Close()
	})
	return d, raw
}

// committed counts the rows another connection sees.
func committed(t *testing.T, raw *sql.DB) int {
	t.Helper()
	var n int
	if err := raw.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (d *driver) queryStatus() string { return ansi.Strip(d.m.query.Status()) }

func TestOpenTransactionIsShownAndAskedAbout(t *testing.T) {
	d, raw := txDriver(t)
	d.runSQL("SELECT 1")
	if cur := d.m.query.current(); cur.tx.Open || strings.Contains(d.queryStatus(), "transaction") {
		t.Fatalf("a transaction after a plain SELECT: %+v", cur.tx)
	}

	d.runSQL("BEGIN; INSERT INTO t VALUES (1)")
	if !strings.Contains(d.queryStatus(), "in transaction") {
		t.Fatalf("no marker after BEGIN: %q", d.queryStatus())
	}
	if n := committed(t, raw); n != 0 {
		t.Fatalf("outside sees %d rows before the commit", n)
	}

	// Closing the tab asks; cancelling keeps the tab and the transaction.
	d.key("ctrl+w")
	if _, ok := d.m.modal.(*txDialog); !ok {
		t.Fatalf("no question:\n%s", d.screen())
	}
	screen := d.screen()
	for _, w := range []string{"Open transaction", "This tab has a transaction that is not committed", "shop", "c commit", "r roll back"} {
		if !strings.Contains(screen, w) {
			t.Errorf("dialog lacks %q:\n%s", w, screen)
		}
	}
	if place, _ := d.m.hereScopes(); place != "Open transaction" {
		t.Errorf("help place %q", place)
	}
	tabs := len(d.m.query.tabs)
	d.key("esc")
	if d.m.modal != nil || len(d.m.query.tabs) != tabs || !d.m.query.current().tx.Open {
		t.Fatal("cancel did not keep the tab and its transaction")
	}

	// Rolling back goes ahead with closing: the unsaved text asks next.
	d.key("ctrl+w", "r")
	if _, ok := d.m.modal.(confirmCloseTab); !ok {
		t.Fatalf("after the rollback, no unsaved-text question: %T", d.m.modal)
	}
	d.key("d")
	if len(d.m.query.tabs) != tabs-1 {
		t.Fatal("the tab was not closed")
	}
	if n := committed(t, raw); n != 0 {
		t.Errorf("rolled back, yet outside sees %d rows", n)
	}
	if _, err := raw.Exec("INSERT INTO t VALUES (0); DELETE FROM t"); err != nil {
		t.Errorf("the closed tab still holds a lock: %v", err)
	}
}

func TestQuitCommitsWhenAsked(t *testing.T) {
	d, raw := txDriver(t)
	d.runSQL("BEGIN; INSERT INTO t VALUES (2)")

	// Quit asks; committing goes ahead and quits.
	next, cmd := d.m.Update(keyPress("ctrl+q"))
	d.m = next.(Model)
	if _, ok := d.m.modal.(*txDialog); !ok || cmd != nil {
		t.Fatalf("quit did not ask: %T", d.m.modal)
	}
	// Commit: the dialog ends the transaction in the background, then quits.
	next, cmd = d.m.Update(keyPress("c"))
	d.m = next.(Model)
	ended := cmd()
	next, cmd = d.m.Update(ended)
	d.m = next.(Model)
	if cmd == nil {
		t.Fatal("no quit after the commit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("the commit did not go on to quit")
	}
	if d.m.modal != nil || d.m.query.current().tx.Open {
		t.Fatalf("after commit: modal %T, tx %+v", d.m.modal, d.m.query.current().tx)
	}
	if n := committed(t, raw); n != 1 {
		t.Errorf("committed, yet outside sees %d rows", n)
	}

	// Without a transaction, quitting does not ask.
	next, cmd = d.m.Update(keyPress("ctrl+q"))
	d.m = next.(Model)
	if d.m.modal != nil || cmd == nil {
		t.Error("quit asked without a transaction")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("quit did not quit")
	}
}

func TestContextChangeAndDisconnectAsk(t *testing.T) {
	d, raw := txDriver(t)
	tab := d.m.query.current()
	d.runSQL("BEGIN; INSERT INTO t VALUES (3)")

	other := d.m.explorer.ConnectionByName("other")
	d.send(contextChosenMsg{tab: tab, ctx: queryContext{conn: other}})
	if _, ok := d.m.modal.(*txDialog); !ok {
		t.Fatalf("changing context did not ask: %T", d.m.modal)
	}
	d.key("esc")
	if tab.ctx.conn.cfg().Name != "shop" {
		t.Error("cancel changed the context anyway")
	}

	d.send(disconnectMsg{tab.ctx.conn})
	if _, ok := d.m.modal.(*txDialog); !ok {
		t.Fatalf("disconnecting did not ask: %T", d.m.modal)
	}
	d.key("r")
	if d.m.modal != nil || tab.tx.Open || committed(t, raw) != 0 {
		t.Errorf("after rollback and disconnect: modal %T, tx %+v", d.m.modal, tab.tx)
	}
}

func TestFailedTransactionOnlyRollsBack(t *testing.T) {
	d, _ := txDriver(t)
	d.runSQL("BEGIN")
	tab := d.m.query.current()
	tab.tx = db.TxState{Open: true, Failed: true, Levels: 1} // as PostgreSQL reports one
	if !strings.Contains(d.queryStatus(), "failed transaction: roll back") {
		t.Errorf("status %q", d.queryStatus())
	}
	d.key("ctrl+w")
	if strings.Contains(d.screen(), "c commit") {
		t.Error("commit offered for a failed transaction")
	}
	d.key("c")
	if !strings.Contains(d.screen(), "cannot be committed") {
		t.Errorf("no explanation:\n%s", d.screen())
	}
}

// A connection lost inside a transaction is reported, not retried on a
// fresh session, where the statement would run outside the transaction.
func TestLostConnectionInsideATransaction(t *testing.T) {
	d, raw := txDriver(t)
	d.runSQL("BEGIN; INSERT INTO t VALUES (4)")
	tab := d.m.query.current()
	db.CloseSession(tab.session) // the connection goes away

	s := d.runSQL("INSERT INTO t VALUES (5)")
	if !strings.Contains(s, "lost inside a transaction") {
		t.Errorf("results:\n%s", s)
	}
	if n := committed(t, raw); n != 0 {
		t.Errorf("the statement ran outside the transaction: %d rows", n)
	}
	if tab.session != nil || tab.tx.Open {
		t.Errorf("session %v, tx %+v after the loss", tab.session, tab.tx)
	}
	// The next run uses a fresh session, outside any transaction.
	d.runSQL("INSERT INTO t VALUES (6)")
	if n := committed(t, raw); n != 1 {
		t.Errorf("after a fresh session: %d rows", n)
	}
}

func keyPress(k string) tea.KeyPressMsg {
	msg := tea.KeyPressMsg{}
	if rest, ok := strings.CutPrefix(k, "ctrl+"); ok {
		msg.Mod, k = tea.ModCtrl, rest
	}
	msg.Code = []rune(k)[0]
	return msg
}
