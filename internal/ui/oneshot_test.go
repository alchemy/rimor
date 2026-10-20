package ui

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jackc/pgx/v5/pgconn"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// oneShot starts rimor as "rimor FILE" would, with one saved connection.
func oneShot(t *testing.T) (*driver, db.Store, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "app.db")
	c, _ := sql.Open("sqlite", file)
	c.Exec("CREATE TABLE things (id INTEGER PRIMARY KEY, name TEXT)")
	c.Close()
	store := db.Store{Path: filepath.Join(dir, "connections.json")}
	store.Save([]db.Config{{Name: "saved one", Driver: db.SQLite, DSN: file}})

	cfg, err := db.ParseTarget(file)
	if err != nil {
		t.Fatal(err)
	}
	settings := config.Defaults()
	settings.Icons = config.IconsPlain
	d := &driver{t: t, m: New(store, "", settings).WithTemporary(cfg), wait: 5 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	for _, cmd := range d.m.startup {
		d.run(cmd)
	}
	t.Cleanup(d.m.explorer.Close)
	return d, store, file
}

func TestOneShotSession(t *testing.T) {
	d, store, file := oneShot(t)

	roots := d.m.explorer.roots
	if len(roots) != 2 || !roots[0].conn.temporary || roots[0].conn.cfg().Name != "app.db" || !roots[0].loaded {
		t.Fatalf("roots: %d, first %+v", len(roots), roots[0].conn.cfg())
	}
	if tree := d.tree(); !strings.Contains(tree, "app.db  temporary") || !strings.Contains(tree, "Tables") {
		t.Errorf("tree:\n%s", tree)
	}
	cur := d.m.query.current()
	if len(d.m.query.tabs) != 1 || cur.ctx.conn != roots[0].conn || d.m.focus != focusQuery {
		t.Errorf("tab on %v, focus %v", cur.ctx.conn, d.m.focus)
	}

	// It runs, like any connection.
	if s := d.runSQL("SELECT COUNT(*) AS n FROM things"); !strings.Contains(s, " n ") && !strings.Contains(s, "n\n") {
		t.Logf("result:\n%s", s)
	}
	if r := d.m.query.current().run; r == nil || r.err != nil {
		t.Fatalf("run: %+v", r)
	}

	// Saving the connections leaves it out.
	d.m.explorer.save()
	saved, _ := store.Load()
	if len(saved) != 1 || saved[0].Name != "saved one" {
		t.Errorf("saved %+v", saved)
	}

	// The form keeps it: then it is an ordinary connection.
	d.key("alt+1", "home", "down", "e")
	if f, ok := d.m.modal.(*connForm); !ok || !strings.Contains(d.screen(), "Save connection") || f.conn != roots[0].conn {
		t.Fatalf("form:\n%s", d.screen())
	}
	d.send(saveConnectionMsg{conn: roots[0].conn, cfg: db.Config{Name: "app", Driver: db.SQLite, DSN: file}})
	saved, _ = store.Load()
	if len(saved) != 2 || roots[0].conn.temporary || strings.Contains(d.tree(), "temporary") {
		t.Errorf("after saving: %+v", saved)
	}
}

func TestOneShotPassword(t *testing.T) {
	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	cfg, _ := db.ParseTarget("postgres://me@127.0.0.1:1/erp?sslmode=disable&connect_timeout=1")
	d := &driver{t: t, m: New(store, "", config.Defaults()).WithTemporary(cfg)}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	root := d.m.explorer.roots[0]
	refused := &pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "me"`}

	// The server refuses a login without a password: rimor asks.
	d.send(loadedMsg{node: root, gen: root.gen, err: refused})
	_, ok := d.m.modal.(*passwordPrompt)
	if !ok {
		t.Fatalf("no password prompt:\n%s", d.screen())
	}
	if s := d.screen(); !strings.Contains(s, "The server wants a password for 127.0.0.1 › erp") || !strings.Contains(s, "Kept in memory only") {
		t.Errorf("prompt:\n%s", s)
	}
	if place, _ := d.m.hereScopes(); place != "Password" {
		t.Errorf("help place %q", place)
	}
	d.typeText("s3cret")
	if strings.Contains(d.screen(), "s3cret") {
		t.Error("the password shows on screen")
	}

	// enter connects again with it, in memory only.
	next, cmd := d.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	d.m = next.(Model)
	if cmd == nil {
		t.Fatal("enter did nothing")
	}
	d.send(cmd())
	got := root.conn.cfg().DSN
	if !strings.Contains(got, "me:s3cret@") || d.m.modal != nil {
		t.Errorf("dsn %q, modal %v", got, d.m.modal)
	}
	saved, _ := store.Load()
	if len(saved) != 0 {
		t.Errorf("a temporary connection was saved: %+v", saved)
	}

	// Refused again: asked again, saying so.
	d.send(loadedMsg{node: root, gen: root.gen, err: refused})
	if _, ok := d.m.modal.(*passwordPrompt); !ok || !strings.Contains(d.screen(), "That password was refused.") {
		t.Errorf("second prompt:\n%s", d.screen())
	}
	d.key("esc")

	// Other failures do not ask.
	d.send(loadedMsg{node: root, gen: root.gen, err: &pgconn.PgError{Code: "3D000", Message: "database does not exist"}})
	if d.m.modal != nil {
		t.Error("asked for a password on another error")
	}
}
