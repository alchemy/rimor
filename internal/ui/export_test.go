package ui

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
	"rimor.dev/internal/export"
)

// exportDriver opens rimor, with a kept session (so export choices are
// remembered), on an SQLite file with a customers table, in a tab on it.
func exportDriver(t *testing.T, settings config.Settings) (*driver, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "shop.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT, note TEXT);
		INSERT INTO customers VALUES (1, 'Ada', NULL), (2, 'Grace; "Amazing"', 'line
two'), (3, 'Linus', 'x')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	store := db.Store{Path: filepath.Join(dir, "c.json")}
	store.Save([]db.Config{{Name: "shop", Driver: db.SQLite, DSN: path}})
	d := &driver{t: t, m: New(store, filepath.Join(dir, "state", "session.json"), settings), wait: 10 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 34})
	d.key("down", "ctrl+t")
	t.Cleanup(func() {
		for _, tb := range d.m.query.tabs {
			if tb.session != nil {
				db.CloseSession(tb.session)
			}
		}
		d.m.explorer.Close()
	})
	return d, dir
}

func (d *driver) exportDialog() *exportDialog {
	d.t.Helper()
	ed, ok := d.m.modal.(*exportDialog)
	if !ok {
		d.t.Fatalf("no export dialog: %T\n%s", d.m.modal, d.screen())
	}
	return ed
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExportToFiles(t *testing.T) {
	t.Setenv("LC_ALL", "it_IT.UTF-8") // a decimal-comma locale: CSV takes ';'
	d, dir := exportDriver(t, config.Defaults())
	d.runSQL("SELECT id, name, note FROM customers ORDER BY id")
	d.key("alt+3", "e")
	ed := d.exportDialog()
	if ed.format != export.CSV || ed.sep != ';' && export.ListSeparator() == ';' {
		t.Fatalf("defaults: format %v, separator %q", ed.format, ed.sep)
	}
	screen := d.screen()
	for _, w := range []string{"Export", "3 rows, 3 columns", "Format", "CSV", "Separator", "; semicolon", "NULL as", "File"} {
		if !strings.Contains(screen, w) {
			t.Errorf("dialog lacks %q:\n%s", w, screen)
		}
	}
	if place, _ := d.m.hereScopes(); place != "Export" {
		t.Errorf("help place %q", place)
	}

	csvPath := filepath.Join(dir, "people.csv")
	ed.file.SetValue(csvPath)
	d.key("enter")
	if d.m.modal != nil {
		t.Fatalf("still open: %s", ansiStatus(ed))
	}
	// RFC 4180 line ends, inside quoted values too; CSV readers give the
	// value back as it was.
	want := "id;name;note\r\n1;Ada;\r\n2;\"Grace; \"\"Amazing\"\"\";\"line\r\ntwo\"\r\n3;Linus;x\r\n"
	if got := readFile(t, csvPath); got != want {
		t.Errorf("CSV:\n%q\nwant:\n%q", got, want)
	}
	if !strings.Contains(d.screen(), "Exported 3 rows to people.csv") {
		t.Errorf("no note:\n%s", d.screen())
	}

	// The comma is one key away, and JSON one more; the extension follows.
	d.key("e")
	ed = d.exportDialog()
	d.key("down", "right") // Separator: the other one
	if ed.sep != ',' {
		t.Errorf("separator %q", ed.sep)
	}
	d.key("up", "right", "right") // Format: CSV → TSV → JSON
	if ed.format != export.JSON || filepath.Ext(ed.file.Value()) != ".json" {
		t.Errorf("format %v, file %q", ed.format, ed.file.Value())
	}
	ed.file.SetValue(filepath.Join(dir, "people.json"))
	d.key("enter")
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "people.json"))), &parsed); err != nil || len(parsed) != 3 {
		t.Fatalf("JSON: %v %v", err, parsed)
	}
	if parsed[0]["id"] != float64(1) || parsed[0]["note"] != nil {
		t.Errorf("JSON values: %v", parsed[0])
	}

	// The choices are remembered: JSON, the comma, the folder.
	d.key("e")
	ed = d.exportDialog()
	if ed.format != export.JSON || ed.sep != ',' || filepath.Dir(absPath(ed.file.Value())) != dir {
		t.Errorf("not remembered: %v %q %q", ed.format, ed.sep, ed.file.Value())
	}

	// An existing file is replaced only on a second enter.
	ed.file.SetValue(filepath.Join(dir, "people.json"))
	d.key("enter")
	if d.m.modal == nil || !strings.Contains(ed.status, "exists") {
		t.Fatalf("no warning: %q", ed.status)
	}
	d.key("enter")
	if d.m.modal != nil {
		t.Error("the second enter did not replace the file")
	}
}

func ansiStatus(ed *exportDialog) string { return ed.status }

func TestExportToClipboardAndInserts(t *testing.T) {
	d, _ := exportDriver(t, config.Defaults())
	d.runSQL("SELECT * FROM customers ORDER BY id")
	d.key("alt+3", "e")
	ed := d.exportDialog()
	// The table, as editing finds it, names the INSERTs.
	if got := ed.table.Value(); got != "customers" {
		t.Errorf("table %q", got)
	}
	for ed.format != export.SQL {
		d.key("right")
	}
	for ed.field != efTo {
		d.key("down")
	}
	d.key("right") // the clipboard
	next, cmd := d.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	d.m = next.(Model)
	if cmd == nil {
		t.Fatal("no export")
	}
	done, ok := cmd().(exportDoneMsg)
	if !ok || done.err != nil {
		t.Fatalf("export: %+v", done)
	}
	if !strings.HasPrefix(done.text, "INSERT INTO customers (id, name, note) VALUES\n    (1, 'Ada', NULL),") {
		t.Errorf("INSERTs:\n%s", done.text)
	}
	next, cmd = d.m.Update(done)
	d.m = next.(Model)
	if cmd == nil || !strings.Contains(d.m.results.notice, "Copied 3 rows as SQL") {
		t.Errorf("clipboard: cmd %v, notice %q", cmd != nil, d.m.results.notice)
	}
}

// A result stopped at the memory limit: every row can still go to the
// file, by running the statement again.
func TestExportRunsAgainForAllRows(t *testing.T) {
	settings := config.Defaults()
	settings.ResultMemoryMB = 1
	d, dir := exportDriver(t, settings)
	d.runSQL(countingSQL(100_000))
	d.settle()
	set := d.m.query.current().run.set()
	if p := set.Progress(); p.Stopped != db.StopLimit {
		t.Fatalf("fixture: not stopped at the limit: %+v", p)
	}
	kept := set.Len()

	d.key("alt+3", "e")
	ed := d.exportDialog()
	if !strings.Contains(d.screen(), "fetching stopped at the memory limit") {
		t.Errorf("no warning:\n%s", d.screen())
	}
	for ed.field != efRows {
		d.key("down")
	}
	d.key("right") // all: run it again
	for ed.format != export.CSV {
		d.key("up", "right")
	}
	path := filepath.Join(dir, "all.csv")
	ed.file.SetValue(path)
	d.key("enter")
	if d.m.modal != nil {
		t.Fatalf("not done: %q", ed.status)
	}
	lines := strings.Count(readFile(t, path), "\n")
	if lines != 100_001 || kept >= 100_000 {
		t.Errorf("%d lines, from a result of %d rows", lines, kept)
	}
	if d.m.query.current().exporting {
		t.Error("the tab is still busy")
	}

	// A statement that writes is not run again.
	d.runSQL("CREATE TABLE big (i INTEGER)")
	d.runSQL("INSERT INTO big (i) WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 100000) SELECT i FROM c RETURNING i, 'row ' || i AS label")
	d.settle()
	if p := d.m.query.current().run.set().Progress(); p.Stopped != db.StopLimit {
		t.Fatalf("fixture: the INSERT's rows were not cut: %+v", p)
	}
	d.key("alt+3", "e")
	for _, f := range d.exportDialog().fields() {
		if f == efRows {
			t.Error("offered to run a statement that writes again")
		}
	}
}

func TestExportDecimalComma(t *testing.T) {
	t.Setenv("LC_ALL", "it_IT.UTF-8")
	if export.DecimalSeparator() != ',' || export.ListSeparator() != ';' {
		t.Skip("the system's region settings decide here")
	}
	d, dir := exportDriver(t, config.Defaults())
	d.runSQL("SELECT 120.5 AS amount, '1.2.3' AS version")
	d.key("alt+3", "e")
	ed := d.exportDialog()
	if !strings.Contains(d.screen(), "decimals 120,5") {
		t.Errorf("no note:\n%s", d.screen())
	}
	path := filepath.Join(dir, "it.csv")
	ed.file.SetValue(path)
	d.key("enter")
	if got := readFile(t, path); got != "amount;version\r\n120,5;1.2.3\r\n" {
		t.Errorf("';': %q", got)
	}

	// The comma separator keeps the dot, and the note goes.
	d.key("e")
	ed = d.exportDialog()
	d.key("down", "right")
	if strings.Contains(d.screen(), "decimals 120,5") {
		t.Error("the note stayed with ','")
	}
	ed.file.SetValue(filepath.Join(dir, "en.csv"))
	d.key("enter")
	if got := readFile(t, filepath.Join(dir, "en.csv")); got != "amount,version\r\n120.5,1.2.3\r\n" {
		t.Errorf("',': %q", got)
	}
}
