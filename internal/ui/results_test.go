package ui

import (
	"database/sql"
	"fmt"
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

// queryDriver opens rimor on a single saved connection with one query tab
// in its context.
func queryDriver(t *testing.T, cfg db.Config, wait time.Duration) *driver {
	t.Helper()
	store := db.Store{Path: filepath.Join(t.TempDir(), "connections.json")}
	if err := store.Save([]db.Config{cfg}); err != nil {
		t.Fatal(err)
	}
	d := &driver{t: t, m: New(store, "", config.Defaults()), wait: wait}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 36})
	d.key("down", "ctrl+t") // select the connection, open a tab there
	if c := d.m.query.current().ctx; c.conn == nil {
		t.Fatal("tab has no context")
	}
	return d
}

// runSQL replaces the tab's text and runs it.
func (d *driver) runSQL(query string) string {
	d.key("alt+2", "ctrl+a")
	d.send(tea.PasteMsg{Content: query})
	d.key("ctrl+enter")
	if r := d.m.query.current().run; r == nil || r.running {
		d.t.Fatalf("run did not finish: %+v", r)
	}
	return d.results()
}

// results renders the results pane body as plain text.
func (d *driver) results() string {
	return ansi.Strip(d.m.results.View(d.m.query.current(), "", d.m.focus == focusResults))
}

func TestResultsSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shop.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for i := range 30 {
		cols = append(cols, fmt.Sprintf("col_%02d TEXT DEFAULT 'value %02d'", i, i))
	}
	for _, stmt := range []string{
		`CREATE TABLE customer (id INTEGER PRIMARY KEY, name TEXT, balance REAL)`,
		`INSERT INTO customer VALUES (1, 'Ada', 10.5), (2, NULL, 2000), (3, 'Grace', NULL)`,
		`CREATE TABLE wide (` + strings.Join(cols, ", ") + `)`,
		`INSERT INTO wide DEFAULT VALUES`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()

	d := queryDriver(t, db.Config{Name: "shop", Driver: db.SQLite, DSN: path}, 0)

	s := d.runSQL("SELECT * FROM customer")
	t.Log("\n" + s)
	for _, want := range []string{"id", "name", "balance", "Ada", "NULL", "2000", "10.5"} {
		if !strings.Contains(s, want) {
			t.Errorf("rows lack %q", want)
		}
	}
	if st := ansi.Strip(d.m.results.Status(d.m.query.current(), "")); !strings.Contains(st, "3 rows") {
		t.Errorf("status = %q", st)
	}

	s = d.runSQL("UPDATE customer SET name = 'x' WHERE id < 3")
	if !strings.Contains(s, "2 rows affected") {
		t.Errorf("affected view:\n%s", s)
	}

	s = d.runSQL("SELECT *\nFORM customer")
	t.Log("\n" + s)
	if !strings.Contains(s, "Query failed") || !strings.Contains(s, "syntax error") {
		t.Errorf("error view:\n%s", s)
	}

	// Wide result: jump to the last column; the first scrolls out of view.
	s = d.runSQL("SELECT * FROM wide")
	if !strings.Contains(s, "col_00") || strings.Contains(s, "col_29") || !strings.Contains(s, "›") {
		t.Errorf("wide result start:\n%s", s)
	}
	d.key("alt+3", "end")
	s = d.results()
	t.Log("\n" + s)
	if strings.Contains(s, "col_00") || !strings.Contains(s, "col_29") || !strings.Contains(s, "‹") {
		t.Errorf("wide result end:\n%s", s)
	}
	if f := ansi.Strip(d.m.results.Footer(d.m.query.current(), true)); !strings.Contains(f, "col_29") {
		t.Errorf("footer = %q", f)
	}
	for i, row := range strings.Split(d.m.render(), "\n") {
		if w := ansi.StringWidth(row); w != d.m.width {
			t.Errorf("row %d is %d wide", i, w)
		}
	}

	// Running only the selection.
	d.key("alt+2", "ctrl+a")
	d.send(tea.PasteMsg{Content: "SELECT 'one' AS a;\nSELECT 'two' AS b"})
	d.key("shift+up", "home", "ctrl+enter") // selects line 2 only
	if s := d.results(); !strings.Contains(s, "two") || strings.Contains(s, "one") {
		t.Errorf("selection run:\n%s", s)
	}
	d.m.Close()
}

func TestResultsPostgresErrorCaret(t *testing.T) {
	dsn := os.Getenv("RIMOR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("RIMOR_TEST_POSTGRES not set")
	}
	d := queryDriver(t, db.Config{Name: "pg", Driver: db.Postgres, DSN: dsn}, 5*time.Second)
	s := d.runSQL("SELECT 1,\n       2 FORM nothing")
	t.Log("\n" + s)
	for _, want := range []string{"42601", `syntax error at or near "nothing"`, "2 │        2 FORM nothing", "^"} {
		if !strings.Contains(s, want) {
			t.Errorf("error view lacks %q", want)
		}
	}
	// The caret sits under "nothing": Postgres reads FORM as a column alias.
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.Contains(l, "2 FORM") && i+1 < len(lines) {
			if strings.Index(lines[i+1], "^") != strings.Index(l, "nothing") {
				t.Errorf("caret misplaced:\n%s\n%s", l, lines[i+1])
			}
		}
	}

	// Session state survives between runs.
	d.runSQL("CREATE TEMP TABLE scratch AS SELECT 7 AS n")
	if s := d.runSQL("SELECT n FROM scratch"); !strings.Contains(s, "7") {
		t.Errorf("temp table lost:\n%s", s)
	}
	d.m.Close()
}

func TestResultsSQLServer(t *testing.T) {
	dsn := os.Getenv("RIMOR_TEST_SQLSERVER")
	if dsn == "" {
		t.Skip("RIMOR_TEST_SQLSERVER not set")
	}
	d := queryDriver(t, db.Config{Name: "erp", Driver: db.SQLServer, DSN: dsn}, 10*time.Second)

	s := d.runSQL("SELECT TOP 25 * FROM sys.objects ORDER BY object_id")
	t.Log("\n" + s)
	if !strings.Contains(s, "object_id") || !strings.Contains(s, "›") {
		t.Errorf("rows view:\n%s", s)
	}

	s = d.runSQL("SELECT 1 AS a,\n  2 AS b,\n  3 AS FROM")
	t.Log("\n" + s)
	if !strings.Contains(s, "Query failed") || !strings.Contains(s, "3 │") {
		t.Errorf("error view:\n%s", s)
	}

	d.runSQL("CREATE TABLE #rimor (a int)")
	if s := d.runSQL("INSERT INTO #rimor VALUES (1), (2)"); !strings.Contains(s, "2 rows affected") {
		t.Errorf("insert:\n%s", s)
	}
	if s := d.runSQL("SELECT COUNT(*) AS n, NEWID() AS id FROM #rimor"); !strings.Contains(s, "2") {
		t.Errorf("temp table lost:\n%s", s)
	}
	d.m.Close()
}
