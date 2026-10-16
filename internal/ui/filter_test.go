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

// filterDriver opens rimor on two SQLite connections, the first expanded
// down to its tables, with the Columns of orders loaded and then closed.
func filterDriver(t *testing.T) *driver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shop.db")
	c, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, delivery_date TEXT)`,
		`CREATE TABLE order_lines (order_id INTEGER, qty INTEGER)`,
	} {
		if _, err := c.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	store.Save([]db.Config{
		{Name: "shop", Driver: db.SQLite, DSN: path},
		{Name: "Orders archive", Driver: db.SQLite, DSN: path},
	})
	settings := config.Defaults()
	settings.Icons = config.IconsPlain
	d := &driver{t: t, m: New(store, "", settings), wait: 5 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 140, Height: 40})
	t.Cleanup(d.m.explorer.Close)

	d.key("down", "enter")                       // open shop
	d.key("down", "enter")                       // its Tables
	d.key("down", "down", "down", "enter")       // orders (customers, order_lines, orders)
	d.key("down", "enter", "enter")              // load its Columns, then close them
	d.key("up", "enter", "up", "up", "up", "up") // close orders; back to shop
	return d
}

func (d *driver) tree() string { return ansi.Strip(d.m.explorer.View(true)) }

// rows are the tree's lines without the filter bar, trimmed.
func (d *driver) rows() []string {
	var out []string
	for i, l := range strings.Split(d.tree(), "\n") {
		if d.m.explorer.filtering && i == 0 {
			continue
		}
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestExplorerFilter(t *testing.T) {
	d := filterDriver(t)
	before := d.tree()
	if strings.Contains(before, "delivery_date") {
		t.Fatalf("fixture: the columns should be loaded but closed:\n%s", before)
	}

	d.key("/")
	if !strings.Contains(d.tree(), "/ ▏type to filter") {
		t.Errorf("no filter bar:\n%s", d.tree())
	}
	d.typeText("ORDER")
	got := strings.Join(d.rows(), "\n")
	want := strings.Join([]string{
		"▾ ◈ shop  sqlite",
		"▾ ▫ Tables  3",
		"▸ ◫ order_lines",
		"▸ ◫ orders",
		"▸ ◈ Orders archive  sqlite",
	}, "\n")
	if got != want {
		t.Errorf("filtered:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(d.tree(), "/ ORDER▏  3 matches") {
		t.Errorf("bar:\n%s", d.tree())
	}
	if d.m.explorer.cursor.obj.Name != "order_lines" {
		t.Errorf("cursor on %q, want the first match", nodeName(d.m.explorer.cursor))
	}

	// The arrows skip the rows that are only the path.
	d.key("down", "down")
	if n := d.m.explorer.cursor; !n.root || n.conn.cfg().Name != "Orders archive" {
		t.Errorf("cursor on %q", nodeName(n))
	}
	d.key("down") // stays on the last match
	d.key("up")
	if d.m.explorer.cursor.obj.Name != "orders" {
		t.Errorf("cursor on %q", nodeName(d.m.explorer.cursor))
	}

	// Loaded rows inside closed ones are found too; keys are text here.
	d.key("backspace", "backspace", "backspace", "backspace", "backspace")
	d.typeText("deliv")
	got = strings.Join(d.rows(), "\n")
	if !strings.Contains(got, "· delivery_date  TEXT") || !strings.Contains(got, "▾ ◫ orders") {
		t.Errorf("column in a closed row:\n%s", got)
	}
	d.typeText("q?x")
	if !d.m.explorer.filtering || d.m.explorer.filter != "delivq?x" {
		t.Errorf("filter %q: single-key shortcuts ran", d.m.explorer.filter)
	}
	if !strings.Contains(d.tree(), "no match") || len(d.rows()) != 0 {
		t.Errorf("no match:\n%s", d.tree())
	}
	d.key("down", "up") // nothing to move to

	// esc puts everything back.
	d.key("esc")
	if d.m.explorer.filtering || d.tree() != before {
		t.Errorf("after esc:\n%s\nwant:\n%s", d.tree(), before)
	}

	// enter goes to the match and opens the path to it.
	d.key("/")
	d.typeText("delivery")
	d.key("enter")
	if d.m.explorer.filtering || d.m.explorer.cursor.obj.Name != "delivery_date" {
		t.Fatalf("after enter: filtering %v, cursor %q", d.m.explorer.filtering, nodeName(d.m.explorer.cursor))
	}
	if tree := d.tree(); !strings.Contains(tree, "delivery_date") || !strings.Contains(tree, "customers") {
		t.Errorf("the path is not open, or the tree is still filtered:\n%s", tree)
	}
}

func TestExplorerFilterHelpAndPaste(t *testing.T) {
	d := filterDriver(t)
	d.key("/")
	if place, _ := d.m.hereScopes(); place != "Explorer filter" {
		t.Errorf("help place %q", place)
	}
	d.send(tea.PasteMsg{Content: "cust\nomers"})
	if d.m.explorer.filter != "cust omers" {
		t.Errorf("pasted %q", d.m.explorer.filter)
	}
	d.key("ctrl+u")
	if d.m.explorer.filter != "" || !strings.Contains(d.tree(), "New connection") {
		t.Errorf("after ctrl+u %q:\n%s", d.m.explorer.filter, d.tree())
	}
	if !strings.Contains(ansi.Strip(d.m.explorer.Hints()), "esc close") {
		t.Errorf("hints %q", ansi.Strip(d.m.explorer.Hints()))
	}
	d.key("esc")
	if strings.Contains(d.tree(), "▏") {
		t.Error("the bar stayed")
	}
}
