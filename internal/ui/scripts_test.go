package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestExplorerScripts(t *testing.T) {
	d := filterDriver(t)                  // shop › Tables open: customers, order_lines, orders
	d.key("down", "down", "down", "down") // orders
	if d.m.explorer.cursor.obj.Name != "orders" {
		t.Fatalf("cursor on %q", nodeName(d.m.explorer.cursor))
	}
	if !strings.Contains(stripHints(d.m.explorer.Hints()), "s script") {
		t.Errorf("hints %q", stripHints(d.m.explorer.Hints()))
	}

	d.key("s")
	if _, ok := d.m.modal.(*scriptMenu); !ok {
		t.Fatalf("no menu:\n%s", d.screen())
	}
	screen := d.screen()
	for _, w := range []string{"Scripts · orders", "› Select first 100 rows", "Insert a row  writes", "Drop the table (commented out)"} {
		if !strings.Contains(screen, w) {
			t.Errorf("menu lacks %q:\n%s", w, screen)
		}
	}
	if place, _ := d.m.hereScopes(); place != "Scripts" {
		t.Errorf("help place %q", place)
	}

	// The third script, in a new tab on the table's connection; not run.
	d.key("down", "down", "enter")
	if d.m.modal != nil {
		t.Fatal("the menu stayed open")
	}
	cur := d.m.query.current()
	if cur.title() != "orders.insert" || cur.ctx.conn.cfg().Name != "shop" || cur.run != nil || cur.dirty() {
		t.Errorf("tab %q on %v, run %v, dirty %v", cur.title(), cur.ctx.conn.cfg().Name, cur.run, cur.dirty())
	}
	if !strings.HasPrefix(cur.ed.Text(), "INSERT INTO orders (delivery_date)") {
		t.Errorf("text:\n%s", cur.ed.Text())
	}
	if d.m.focus != focusQuery || !strings.Contains(d.screen(), "⚠ Opened Insert a row: it writes") {
		t.Errorf("focus %v:\n%s", d.m.focus, d.screen())
	}

	// S opens the first script straight away; a column's is its values.
	d.key("alt+1", "S")
	if got := d.m.query.current().title(); got != "orders.select" {
		t.Errorf("S opened %q", got)
	}
	d.key("alt+1", "enter", "down", "enter", "down") // orders › Columns › id
	if n := d.m.explorer.cursor; n.obj.Name != "id" || n.obj.Table != "orders" {
		t.Fatalf("cursor on %q of %q", n.obj.Name, n.obj.Table)
	}
	d.key("S")
	if cur := d.m.query.current(); cur.title() != "orders.values" || !strings.Contains(cur.ed.Text(), "GROUP BY id") {
		t.Errorf("column script %q:\n%s", cur.title(), cur.ed.Text())
	}

	// Rows without scripts ignore the keys.
	d.key("alt+1", "up") // the Columns folder
	tabs := len(d.m.query.tabs)
	d.key("s", "S")
	if d.m.modal != nil || len(d.m.query.tabs) != tabs {
		t.Error("a folder had scripts")
	}
}

func stripHints(s string) string { return strings.Join(strings.Fields(ansi.Strip(s)), " ") }
