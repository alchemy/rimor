package ui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

func TestLeaderKey(t *testing.T) {
	d := layoutDriver(t, "")
	d.key("alt+2")
	d.typeText("SELECT")

	// The leader shows what can follow, and types nothing.
	d.key("ctrl+g")
	s := ansi.Strip(d.m.render())
	for _, want := range []string{"ctrl+g …", "Toggle full screen", "Run the selection", "← h"} {
		if !strings.Contains(s, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	d.key("f")
	if !d.m.split.full || d.m.leader || d.m.query.current().ed.Text() != "SELECT" {
		t.Fatalf("leader f: full=%v leader=%v text=%q", d.m.split.full, d.m.leader, d.m.query.current().ed.Text())
	}
	d.key("ctrl+g", "f")
	if d.m.split.full {
		t.Fatal("leader f did not toggle back")
	}

	d.key("ctrl+g", "1")
	if d.m.focus != focusExplorer {
		t.Errorf("leader 1 focus = %d", d.m.focus)
	}
	d.key("ctrl+g", "shift+3")
	if d.m.focus != focusResults || !d.m.split.full {
		t.Errorf("leader shift+3: focus %d full %v", d.m.focus, d.m.split.full)
	}
	d.key("ctrl+g", "f")

	// Unknown keys and esc end the sequence without doing anything.
	d.key("alt+2", "ctrl+g", "x")
	d.key("ctrl+g", "esc")
	if d.m.leader || d.m.query.current().ed.Text() != "SELECT" {
		t.Errorf("leader leaked: leader=%v text=%q", d.m.leader, d.m.query.current().ed.Text())
	}
}

func TestLeaderResizeRepeats(t *testing.T) {
	d := layoutDriver(t, "")
	start, _, _ := d.m.layout()
	d.key("ctrl+g", "right", "right", "l")
	if got, _, _ := d.m.layout(); got != start+3*resizeStepCols {
		t.Fatalf("width %d, want %d", got, start+3*resizeStepCols)
	}
	if !d.m.leader || !strings.Contains(ansi.Strip(d.m.render()), "resizing") {
		t.Errorf("resize mode not shown")
	}
	d.key("esc")
	if d.m.leader {
		t.Errorf("esc did not end resize mode")
	}
	d.key("ctrl+g", "=")
	if got, _, _ := d.m.layout(); got != start || d.m.leader {
		t.Errorf("reset: width %d leader %v", got, d.m.leader)
	}
}

func TestRemappedKeys(t *testing.T) {
	s := config.Defaults()
	s.Leader = "ctrl+b"
	s.Keys = map[string][]string{"full_screen": {"ctrl+z", "leader z"}}
	d := &driver{t: t, m: New(db.Store{Path: filepath.Join(t.TempDir(), "c.json")}, "", s)}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 40})

	d.key("alt+f") // no longer bound
	if d.m.split.full {
		t.Fatal("old binding still works")
	}
	d.key("ctrl+z")
	if !d.m.split.full {
		t.Fatal("new binding does not work")
	}
	if s := ansi.Strip(d.m.render()); !strings.Contains(s, "ctrl+z exit full screen") {
		t.Errorf("hint does not follow the binding")
	}
	d.key("ctrl+b", "z")
	if d.m.split.full {
		t.Fatal("new leader binding does not work")
	}
	if s := ansi.Strip(d.m.render()); !strings.Contains(s, "^b keys") {
		t.Errorf("footer does not name the leader")
	}
}

func TestLeaderIgnoredInDialogs(t *testing.T) {
	d := layoutDriver(t, "")
	d.key("alt+1", "a") // the new-connection form
	d.key("ctrl+g")
	if d.m.leader {
		t.Fatal("leader captured inside a dialog")
	}
}
