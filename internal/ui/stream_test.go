package ui

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

func countingSQL(n int) string {
	return fmt.Sprintf(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < %d)
SELECT i, 'row ' || i AS label FROM c`, n)
}

// streamDriver opens rimor on an SQLite file with one tab in its context.
func streamDriver(t *testing.T, settings config.Settings) *driver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.db")
	c, _ := sql.Open("sqlite", path)
	c.Exec("CREATE TABLE t (a int)")
	c.Close()
	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	store.Save([]db.Config{{Name: "s", Driver: db.SQLite, DSN: path}})
	d := &driver{t: t, m: New(store, "", settings), wait: 5 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	d.key("down", "ctrl+t")
	return d
}

func (d *driver) status() string {
	return ansi.Strip(d.m.results.Status(d.m.query.current(), ""))
}

// settle waits for a fetch the driver stopped following to end, then
// delivers its final rowsMsg.
func (d *driver) settle() {
	d.t.Helper()
	r := d.m.query.current().run
	deadline := time.Now().Add(20 * time.Second)
	for !r.res.Rows.Progress().Done {
		if time.Now().After(deadline) {
			d.t.Fatal("fetch did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.send(rowsMsg{d.m.query.current(), r.gen})
}

func TestStreamLargeResult(t *testing.T) {
	d := streamDriver(t, config.Defaults())
	d.runSQL(countingSQL(200_000))
	d.settle()
	if s := d.status(); !strings.Contains(s, "✓ 200,000 rows") {
		t.Fatalf("status = %q", s)
	}
	d.key("alt+3", "G")
	if f := ansi.Strip(d.m.results.Footer(d.m.query.current(), true)); !strings.Contains(f, "200,000/200,000") {
		t.Errorf("footer = %q", f)
	}
	if s := d.results(); !strings.Contains(s, "row 200000") {
		t.Errorf("last row not shown:\n%s", s)
	}
	d.screen() // every row stays the terminal width
	d.m.Close()
}

func TestStreamMemoryLimit(t *testing.T) {
	s := config.Defaults()
	s.ResultMemoryMB = 1
	d := streamDriver(t, s)
	d.runSQL(countingSQL(2_000_000))
	d.settle()
	st := d.status()
	if !strings.Contains(st, "stopped at the 1 MB limit") {
		t.Fatalf("status = %q", st)
	}
	if n := d.m.query.current().run.res.Rows.Len(); n == 0 || n >= 2_000_000 {
		t.Errorf("kept %d rows", n)
	}
	d.m.Close()
}

func TestStreamStopAndDisconnect(t *testing.T) {
	d := streamDriver(t, config.Defaults())
	d.wait = 0 // stop following the fetch, so it is still running below
	d.runSQL(countingSQL(50_000_000))
	r := d.m.query.current().run
	if !r.fetching() {
		t.Fatal("not fetching")
	}
	if s := d.status(); !strings.Contains(s, "fetching") {
		t.Errorf("status while fetching = %q", s)
	}
	if f := ansi.Strip(d.m.results.Footer(d.m.query.current(), true)); !strings.Contains(f, "esc stop") {
		t.Errorf("footer while fetching = %q", f)
	}

	// A new run in the same tab waits for the fetch.
	d.key("ctrl+enter")
	if d.m.query.current().run != r {
		t.Error("a second run started while fetching")
	}

	d.key("alt+3", "esc")
	d.settle()
	if s := d.status(); !strings.Contains(s, "stopped") || r.res.Rows.Len() == 0 {
		t.Errorf("after esc: %q, %d rows", s, r.res.Rows.Len())
	}

	// Start another long fetch, then disconnect: it must stop, not hang.
	d.runSQL(countingSQL(50_000_000))
	r = d.m.query.current().run
	done := make(chan struct{})
	go func() {
		d.key("alt+1", "g", "down", "x") // the connection row, then disconnect
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect blocked")
	}
	d.settle()
	if p := r.res.Rows.Progress(); p.Stopped != db.StopCancelled {
		t.Errorf("fetch not stopped by disconnect: %+v", p)
	}
	d.m.Close()
}
