package ui

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// pgCells opens rimor on Postgres with a fresh cell_items table.
func pgCells(t *testing.T, readOnly bool) (*driver, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("RIMOR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("RIMOR_TEST_POSTGRES not set")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Exec("DROP TABLE IF EXISTS cell_items"); raw.Close() })
	for _, s := range []string{
		`DROP TABLE IF EXISTS cell_items`,
		`CREATE TABLE cell_items (id int PRIMARY KEY, name text, note text)`,
		`INSERT INTO cell_items VALUES (1, 'bolt', NULL), (2, 'nut', '` + strings.Repeat("long ", 40) + `')`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	d := queryDriver(t, db.Config{Name: "pg", Driver: db.Postgres, DSN: dsn, ReadOnly: readOnly}, 5*time.Second)
	return d, raw
}

func (d *driver) popup() *cellPopup {
	p, _ := d.m.modal.(*cellPopup)
	return p
}

// openCell runs the query and opens the cell at row, col.
func (d *driver) openCell(query string, row, col int) *cellPopup {
	d.t.Helper()
	d.runSQL(query)
	d.key("alt+3", "g", "home")
	for range row {
		d.key("down")
	}
	for range col {
		d.key("right")
	}
	d.key("enter")
	p := d.popup()
	if p == nil {
		d.t.Fatalf("no popup:\n%s", d.screen())
	}
	return p
}

func dbValue(t *testing.T, raw *sql.DB, query string) sql.NullString {
	var v sql.NullString
	if err := raw.QueryRowContext(context.Background(), query).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCellEditPostgres(t *testing.T) {
	d, raw := pgCells(t, false)
	p := d.openCell("SELECT id, name, note FROM cell_items ORDER BY id", 0, 1)
	if p.phase != cellEditing {
		t.Fatalf("phase %d, reason %q", p.phase, p.reason)
	}
	if got := p.ed.SelectedText(); got != "bolt" {
		t.Errorf("selected = %q", got)
	}
	d.typeText("hex bolt") // replaces the selection
	s := ansi.Strip(d.m.render())
	t.Log("\n" + s)
	if !strings.Contains(s, "UPDATE public.cell_items SET name = 'hex bolt' WHERE id = '1'") {
		t.Errorf("no statement preview")
	}
	if v := d.m.View(); v.Cursor == nil {
		t.Error("no cursor in the popup")
	}
	d.key("enter")
	if d.popup() != nil {
		t.Fatalf("popup still open: %q", p.err)
	}
	if v := dbValue(t, raw, "SELECT name FROM cell_items WHERE id = 1"); v.String != "hex bolt" {
		t.Errorf("database has %q", v.String)
	}
	if got := d.m.query.current().run.res.Rows.Cell(0, 1); got.Text != "hex bolt" {
		t.Errorf("grid shows %q", got.Text)
	}
	if f := ansi.Strip(d.m.results.Footer(d.m.query.current(), true)); !strings.Contains(f, "saved name") {
		t.Errorf("footer = %q", f)
	}

	// NULL to text with a second line, then back to NULL.
	d.key("right", "enter")
	p = d.popup()
	if !p.null || p.phase != cellEditing {
		t.Fatalf("note should start as NULL: null=%v phase=%d", p.null, p.phase)
	}
	d.typeText("first")
	d.key("alt+enter")
	d.typeText("second")
	d.key("enter")
	if v := dbValue(t, raw, "SELECT note FROM cell_items WHERE id = 1"); v.String != "first\nsecond" {
		t.Errorf("note = %q", v.String)
	}
	d.key("enter", "ctrl+n", "enter")
	if v := dbValue(t, raw, "SELECT note FROM cell_items WHERE id = 1"); v.Valid {
		t.Errorf("note not NULL: %q", v.String)
	}
	if got := d.m.query.current().run.res.Rows.Cell(0, 2); !got.Null {
		t.Errorf("grid shows %+v", got)
	}

	// A value the database rejects keeps the popup open with its error.
	d.key("left", "enter")
	raw.Exec("ALTER TABLE cell_items ADD CONSTRAINT short CHECK (length(name) < 20)")
	d.typeText(strings.Repeat("x", 30))
	d.key("enter")
	if p := d.popup(); p == nil || !strings.Contains(p.err, "short") {
		t.Errorf("rejected value: popup %v", p)
	}
	d.key("esc")

	// Someone else changes the row: the edit is refused, not applied.
	d.key("enter")
	raw.Exec("UPDATE cell_items SET name = 'elsewhere' WHERE id = 1")
	d.typeText("mine")
	d.key("enter")
	if p := d.popup(); p == nil || !strings.Contains(p.reason, "changed since it was loaded") {
		t.Errorf("stale edit: popup %+v", p)
	}
	if v := dbValue(t, raw, "SELECT name FROM cell_items WHERE id = 1"); v.String != "elsewhere" {
		t.Errorf("stale edit applied: %q", v.String)
	}
	d.key("esc")

	// The key column and a long value in the viewer.
	d.key("home", "enter")
	if p := d.popup(); p.phase != cellViewing || !strings.Contains(p.reason, "Key columns") {
		t.Errorf("key column: %+v", p.reason)
	}
	d.key("esc", "down", "end", "enter")
	if p := d.popup(); !strings.Contains(p.ed.Text(), strings.Repeat("long ", 39)) {
		t.Errorf("viewer lost text: %q", p.ed.Text())
	}
	d.key("esc")
	d.m.Close()
}

func TestCellNotEditable(t *testing.T) {
	d, _ := pgCells(t, false)
	p := d.openCell("SELECT i.id, i.name, upper(i.name) AS shout FROM cell_items i JOIN cell_items j ON j.id = i.id", 0, 1)
	// A self-join: both sides are the same table to the database, so an
	// edit could reach the wrong row.
	if p.phase != cellViewing || !strings.Contains(p.reason, "appears 2 times") {
		t.Errorf("self-join: phase %d reason %q", p.phase, p.reason)
	}
	// Typing does nothing in the viewer; copy still works.
	before := p.ed.Text()
	d.typeText("zzz")
	if p.ed.Text() != before {
		t.Errorf("viewer was edited")
	}
	d.key("esc")
	d.m.Close()

	ro, _ := pgCells(t, true)
	p = ro.openCell("SELECT id, name FROM cell_items", 0, 1)
	if !strings.Contains(p.reason, "read-only") {
		t.Errorf("read-only: %q", p.reason)
	}
	ro.key("esc")
	if s := ro.runSQL("DELETE FROM cell_items"); !strings.Contains(s, "read-only") {
		t.Errorf("read-only connection ran a DELETE:\n%s", s)
	}
	ro.m.Close()
}

func TestCellSQLiteViewOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	c, _ := sql.Open("sqlite", path)
	c.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT); INSERT INTO t VALUES (1, 'x')")
	c.Close()
	d := queryDriver(t, db.Config{Name: "s", Driver: db.SQLite, DSN: path}, 0)
	p := d.openCell("SELECT id, a FROM t", 0, 1)
	if p.phase != cellViewing || !strings.Contains(p.reason, "SQLite") {
		t.Errorf("sqlite: phase %d reason %q", p.phase, p.reason)
	}
	if !strings.Contains(ansi.Strip(d.m.render()), "^c copy") {
		t.Errorf("viewer hints missing")
	}
	d.m.Close()
	_ = config.Defaults
}
