package ui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

func updateDriver(t *testing.T) *driver {
	t.Helper()
	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	d := &driver{t: t, m: New(store, "", config.Defaults())}
	return d
}

func TestUpdateNotice(t *testing.T) {
	// Announced: the marker and, once, the note.
	d := updateDriver(t)
	d.m = d.m.WithUpdate(UpdateMsg{Latest: "v0.7.0", URL: "https://github.com/alchemy/rimor/releases/tag/v0.7.0",
		Command: "go install rimor.dev/cmd/rimor@latest", Announce: true})
	d.send(tea.WindowSizeMsg{Width: 140, Height: 30})
	screen := d.screen()
	if !strings.Contains(screen, "↑ v0.7.0") || !strings.Contains(screen, "↑ rimor v0.7.0 is out · go install rimor.dev/cmd/rimor@latest") {
		t.Errorf("announced:\n%s", screen)
	}
	if out := d.m.render(); !strings.Contains(out, "\x1b]8;;https://github.com/alchemy/rimor/releases/tag/v0.7.0") {
		t.Error("the marker does not link to the release")
	}
	d.key("alt+2", "x") // any key in the editor clears the note; the marker stays
	if screen := d.screen(); strings.Contains(screen, "is out") || !strings.Contains(screen, "↑ v0.7.0") {
		t.Errorf("after a key:\n%s", screen)
	}

	// Known before: only the marker. A message during the session works
	// the same way.
	d = updateDriver(t)
	d.send(tea.WindowSizeMsg{Width: 140, Height: 30})
	d.send(UpdateMsg{Latest: "v0.8.0", URL: "https://example.org/r", Announce: false})
	if screen := d.screen(); strings.Contains(screen, "is out") || !strings.Contains(screen, "↑ v0.8.0") {
		t.Errorf("not announced:\n%s", screen)
	}

	// Without a newer release, nothing.
	d = updateDriver(t)
	d.send(tea.WindowSizeMsg{Width: 140, Height: 30})
	if strings.Contains(d.screen(), "↑") {
		t.Error("a marker without an update")
	}
}
