package ui

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/db"
)

func pgJSON(t *testing.T) (*driver, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("RIMOR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("RIMOR_TEST_POSTGRES not set")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Exec("DROP TABLE IF EXISTS json_items"); raw.Close() })
	for _, s := range []string{
		`DROP TABLE IF EXISTS json_items`,
		`CREATE TABLE json_items (id int PRIMARY KEY, doc jsonb, raw json, note text, name text)`,
		`INSERT INTO json_items VALUES (1, '{"name": "bolt", "sizes": [6, 8], "metric": true}',
			'{"a":1,"b":[1,2]}', E'line one\nline two', 'single')`,
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return queryDriver(t, db.Config{Name: "pg", Driver: db.Postgres, DSN: dsn}, 5*time.Second), raw
}

func TestCellJSONB(t *testing.T) {
	d, raw := pgJSON(t)
	p := d.openCell("SELECT id, doc, raw, note, name FROM json_items", 0, 1)
	if !p.json || !p.normalized || !p.multiline || p.phase != cellEditing {
		t.Fatalf("json=%v normalized=%v multiline=%v phase=%d reason=%q", p.json, p.normalized, p.multiline, p.phase, p.reason)
	}
	// Pretty-printed for editing, highlighted, in a large popup.
	// jsonb keeps keys in its own order (shorter first).
	if !strings.Contains(p.ed.Text(), "{\n  \"name\": \"bolt\",\n  \"sizes\": [\n") {
		t.Errorf("not pretty-printed:\n%s", p.ed.Text())
	}
	s := ansi.Strip(d.m.render())
	t.Log("\n" + s)
	if !strings.Contains(s, "^s save") || !strings.Contains(s, "^f format") {
		t.Errorf("JSON hints missing")
	}
	if p.width < 84 {
		t.Errorf("popup width %d", p.width)
	}

	// enter adds a line; typing at the end of the object adds a member.
	d.key("ctrl+end", "up")
	d.key("end")
	d.typeText(",")
	d.key("enter")
	d.typeText(`"pitch": 1.25`)
	if strings.Count(p.ed.Text(), "\n") < 7 || d.popup() == nil {
		t.Fatalf("enter did not add a line:\n%s", p.ed.Text())
	}

	// Invalid JSON is not sent; the cursor goes to the fault.
	d.key("backspace", "backspace", "backspace") // "pitch": 1
	d.typeText(",")                              // trailing comma
	d.key("ctrl+s")
	if p := d.popup(); p == nil || !strings.Contains(p.err, "Invalid JSON at line") {
		t.Fatalf("invalid JSON accepted: %+v", p)
	}
	if line, _ := p.ed.CursorPosition(); line < 5 {
		t.Errorf("cursor not moved to the fault: line %d", line)
	}
	if v := dbValue(t, raw, "SELECT doc->>'pitch' FROM json_items WHERE id = 1"); v.Valid {
		t.Errorf("invalid JSON reached the database")
	}

	// The parser stops at the "}" after the comma; go back and remove it.
	d.key("up", "end", "backspace")
	d.key("ctrl+s")
	if d.popup() != nil {
		t.Fatalf("popup still open: %q", p.err)
	}
	if v := dbValue(t, raw, "SELECT doc->>'pitch' FROM json_items WHERE id = 1"); v.String != "1" {
		t.Errorf("pitch = %q", v.String)
	}
	if got := d.m.query.current().run.res.Rows.Cell(0, 1); !strings.Contains(got.Text, `"pitch": 1`) {
		t.Errorf("grid shows %q", got.Text)
	}
	d.m.Close()
}

func TestCellJSONKeepsStoredText(t *testing.T) {
	d, raw := pgJSON(t)
	// json (not jsonb) stores text as written: it opens unchanged.
	p := d.openCell("SELECT id, doc, raw, note, name FROM json_items", 0, 2)
	if !p.json || p.normalized || p.ed.Text() != `{"a":1,"b":[1,2]}` {
		t.Fatalf("json column: normalized=%v text=%q", p.normalized, p.ed.Text())
	}
	d.key("ctrl+f")
	if !strings.Contains(p.ed.Text(), "\n  \"a\": 1,") {
		t.Fatalf("ctrl+f did not format:\n%s", p.ed.Text())
	}
	d.key("ctrl+s")
	if v := dbValue(t, raw, "SELECT raw::text FROM json_items WHERE id = 1"); !strings.Contains(v.String, "\n  \"a\": 1,") {
		t.Errorf("formatted text not stored: %q", v.String)
	}

	// Multi-line text: enter adds a line, ctrl+s saves.
	d.key("right", "enter")
	p = d.popup()
	if p.json || !p.multiline {
		t.Fatalf("note: json=%v multiline=%v", p.json, p.multiline)
	}
	d.key("ctrl+end", "enter")
	d.typeText("line three")
	d.key("ctrl+s")
	if v := dbValue(t, raw, "SELECT note FROM json_items WHERE id = 1"); v.String != "line one\nline two\nline three" {
		t.Errorf("note = %q", v.String)
	}

	// A single-line value still saves with enter.
	d.key("right", "enter")
	if p := d.popup(); p.multiline {
		t.Fatal("single-line value in multi-line mode")
	}
	d.typeText("double")
	d.key("enter")
	if v := dbValue(t, raw, "SELECT name FROM json_items WHERE id = 1"); v.String != "double" {
		t.Errorf("name = %q", v.String)
	}
	d.m.Close()
}

func TestCellJSONViewer(t *testing.T) {
	d, _ := pgJSON(t)
	// A computed JSON value cannot be edited, but views formatted.
	p := d.openCell("SELECT id, raw::jsonb AS doc FROM json_items", 0, 1)
	if p.phase != cellViewing || !strings.Contains(p.ed.Text(), "\n  \"a\": 1,") {
		t.Fatalf("viewer: phase %d text %q reason %q", p.phase, p.ed.Text(), p.reason)
	}
	d.typeText("x")
	if strings.Contains(p.ed.Text(), "x") {
		t.Error("viewer accepted typing")
	}
	d.m.Close()
}
