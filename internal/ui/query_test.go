package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// TestQueryTabs runs against a Postgres server with at least two databases,
// one of them named in RIMOR_TEST_POSTGRES_DB (default "warehouse").
func TestQueryTabs(t *testing.T) {
	dsn := os.Getenv("RIMOR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("RIMOR_TEST_POSTGRES not set")
	}
	other := os.Getenv("RIMOR_TEST_POSTGRES_DB")
	if other == "" {
		other = "warehouse"
	}

	dir := t.TempDir()
	store := db.Store{Path: filepath.Join(dir, "connections.json")}
	if err := store.Save([]db.Config{{Name: "local", Driver: db.Postgres, DSN: dsn}}); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(dir, "session.json")
	d := &driver{t: t, m: New(store, sessionPath, config.Defaults())}
	d.send(tea.WindowSizeMsg{Width: 140, Height: 32})

	// Select the other database in the explorer and open a tab there.
	d.key("down", "enter")
	for range len(d.m.explorer.Databases(d.m.explorer.Connections()[0])) {
		if c := d.m.explorer.Context(); c.database == other {
			break
		}
		d.key("down")
	}
	if c := d.m.explorer.Context(); c.database != other {
		t.Fatalf("could not select %s; context = %+v", other, c)
	}
	d.key("ctrl+t")
	d.typeText("SELECT 1")
	s := d.screen()
	t.Log("\n" + s)
	for _, want := range []string{"untitled-2 ●", "local › " + other, "SELECT 1", "1:9"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen lacks %q", want)
		}
	}
	if d.m.focus != focusQuery || d.m.query.active != 1 {
		t.Fatalf("focus %d, active tab %d", d.m.focus, d.m.query.active)
	}

	// Save as: ctrl+s on an untitled tab asks for a path.
	path := filepath.Join(dir, "first")
	d.key("ctrl+s")
	if _, ok := d.m.modal.(*pathPrompt); !ok {
		t.Fatalf("no path prompt:\n%s", d.screen())
	}
	for range 200 {
		d.key("backspace")
	}
	d.typeText(path)
	d.key("enter")
	if data, err := os.ReadFile(path + ".sql"); err != nil || string(data) != "SELECT 1" {
		t.Fatalf("saved file: %q %v", data, err)
	}
	if s := d.screen(); !strings.Contains(s, "first.sql") || strings.Contains(s, "first.sql ●") {
		t.Errorf("tab label not updated:\n%s", s)
	}

	// Pick "no connection" in the context picker.
	d.key("ctrl+e")
	if _, ok := d.m.modal.(*contextPicker); !ok {
		t.Fatalf("no picker")
	}
	s = d.screen()
	t.Log("\n" + s)
	if !strings.Contains(s, other) || !strings.Contains(s, "Query context") {
		t.Errorf("picker lacks databases:\n%s", s)
	}
	d.typeText("no conn")
	d.key("enter")
	if c := d.m.query.current().ctx; c.conn != nil {
		t.Fatalf("context = %+v", c)
	}

	// Edit, then close: the dirty tab asks first; discard closes it.
	d.typeText("\n-- more")
	d.key("ctrl+w")
	if _, ok := d.m.modal.(confirmCloseTab); !ok {
		t.Fatalf("no close confirmation")
	}
	d.key("esc")
	if len(d.m.query.tabs) != 2 {
		t.Fatalf("tab closed on esc")
	}

	// Quit and restart: tabs, unsaved text and contexts come back.
	d.m.Close()
	d2 := &driver{t: t, m: New(store, sessionPath, config.Defaults())}
	d2.send(tea.WindowSizeMsg{Width: 140, Height: 32})
	if len(d2.m.query.tabs) != 2 {
		t.Fatalf("restored %d tabs", len(d2.m.query.tabs))
	}
	tb := d2.m.query.tabs[1]
	if tb.path != path+".sql" || !tb.dirty() || !strings.HasSuffix(tb.ed.Text(), "-- more") {
		t.Fatalf("restored tab: path %q dirty %v text %q", tb.path, tb.dirty(), tb.ed.Text())
	}
	if d2.m.query.tabs[0].name != "untitled-1" || d2.m.query.active != 1 {
		t.Fatalf("restored names/active wrong")
	}

	// Open the saved file again: it selects the existing tab.
	d2.key("ctrl+o")
	for range 200 {
		d2.key("backspace")
	}
	d2.typeText(path + ".sql")
	d2.key("enter")
	if len(d2.m.query.tabs) != 2 || d2.m.query.active != 1 {
		t.Fatalf("open duplicated the tab")
	}
	d2.m.Close()
}

func TestQueryOpenReplacesEmptyTab(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "report.sql")
	os.WriteFile(file, []byte("select 42;"), 0o644)
	d := &driver{t: t, m: New(db.Store{Path: filepath.Join(dir, "c.json")}, "", config.Defaults())}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	d.key("ctrl+o")
	for range 200 {
		d.key("backspace")
	}
	d.typeText(filepath.Join(dir, "rep"))
	d.key("tab") // completes to report.sql
	if v := d.m.modal.(*pathPrompt).input.Value(); v != file {
		t.Fatalf("completion = %q", v)
	}
	d.key("enter")
	if len(d.m.query.tabs) != 1 || d.m.query.current().title() != "report.sql" {
		t.Fatalf("tabs = %d, title %q", len(d.m.query.tabs), d.m.query.current().title())
	}
	if s := d.screen(); !strings.Contains(s, "select 42;") {
		t.Errorf("content missing:\n%s", s)
	}
}

func TestContextPickerScrolls(t *testing.T) {
	dir := t.TempDir()
	store := db.Store{Path: filepath.Join(dir, "c.json")}
	var cfgs []db.Config
	for i := range 40 {
		cfgs = append(cfgs, db.Config{Name: fmt.Sprintf("conn-%02d", i), Driver: db.SQLite, DSN: "x.db"})
	}
	if err := store.Save(cfgs); err != nil {
		t.Fatal(err)
	}
	d := &driver{t: t, m: New(store, "", config.Defaults())}
	d.send(tea.WindowSizeMsg{Width: 100, Height: 24})
	d.key("alt+2", "ctrl+e")

	for range 30 {
		d.key("down")
	}
	s := d.screen()
	t.Log("\n" + s)
	if got := strings.Count(s, "\n") + 1; got != 24 {
		t.Fatalf("screen has %d rows", got)
	}
	// The cursor is on item 30: "no connection" plus conn-00..conn-29.
	for _, want := range []string{"conn-29", "↑", "more", "↓"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen lacks %q", want)
		}
	}
	dialog := func() string { return ansi.Strip(d.m.modal.View(d.m.dialogWidth())) }
	if strings.Contains(dialog(), "conn-00") {
		t.Errorf("first item should have scrolled away")
	}
	d.key("end")
	if s := dialog(); !strings.Contains(s, "conn-39") || strings.Contains(s, "↓") {
		t.Errorf("end does not reach the last item:\n%s", s)
	}
}
