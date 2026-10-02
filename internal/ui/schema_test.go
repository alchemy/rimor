package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// Uses the fixture schemas of the db tests: staging is empty.
func TestExplorerHidesEmptySchemas(t *testing.T) {
	dsn := os.Getenv("RIMOR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("RIMOR_TEST_POSTGRES not set")
	}
	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	store.Save([]db.Config{{Name: "pg", Driver: db.Postgres, DSN: dsn}})
	d := &driver{t: t, m: New(store, "", config.Defaults()), wait: 5 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 140, Height: 50})

	// Open the connection and its default database.
	d.key("down", "enter")
	for d.m.explorer.cursor.obj.Detail != "default" {
		d.key("down")
	}
	db0 := d.m.explorer.cursor
	d.key("enter")
	tree := func() string { return ansi.Strip(d.m.explorer.View(true)) }

	s := tree()
	t.Log("\n" + s)
	flat := strings.Join(strings.Fields(s), " ") // the note wraps in a narrow pane
	if strings.Contains(s, "staging") || !strings.Contains(flat, "1 schema with nothing visible · . shows") {
		t.Errorf("empty schema not hidden:\n%s", s)
	}
	for _, want := range []string{"public", "sales", "types_only", "funcs_only", "seq_only"} {
		if !strings.Contains(s, want) {
			t.Errorf("schema %s missing", want)
		}
	}
	if h := ansi.Strip(d.m.explorer.Hints()); !strings.Contains(h, ". schemas") {
		t.Errorf("hints = %q", h)
	}

	// "." shows them, muted and marked; again hides them. It works from
	// anywhere inside the database.
	d.key("down", ".")
	s = tree()
	if !strings.Contains(s, "staging  empty") || !strings.Contains(s, "default  + empty") || strings.Contains(s, "nothing") {
		t.Errorf("show all:\n%s", s)
	}

	// Hiding the schema under the cursor moves the cursor to the database.
	for d.m.explorer.cursor.obj.Name != "staging" {
		d.key("down")
	}
	d.key(".")
	if d.m.explorer.cursor != db0 {
		t.Errorf("cursor = %q, want the database", d.m.explorer.cursor.obj.Name)
	}
	if strings.Contains(tree(), "staging") {
		t.Errorf("toggle did not hide again")
	}
	d.m.Close()
}
