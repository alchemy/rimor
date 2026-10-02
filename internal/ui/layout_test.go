package ui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

func (d *driver) click(x, y int) {
	d.send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
}

func (d *driver) drag(fromX, fromY, toX, toY int) {
	d.click(fromX, fromY)
	d.send(tea.MouseMotionMsg{X: (fromX + toX) / 2, Y: (fromY + toY) / 2, Button: tea.MouseLeft})
	d.send(tea.MouseMotionMsg{X: toX, Y: toY, Button: tea.MouseLeft})
	d.send(tea.MouseReleaseMsg{X: toX, Y: toY, Button: tea.MouseLeft})
}

func layoutDriver(t *testing.T, sessionPath string) *driver {
	d := &driver{t: t, m: New(db.Store{Path: filepath.Join(t.TempDir(), "c.json")}, sessionPath, config.Defaults())}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	return d
}

func TestDragExplorerHandle(t *testing.T) {
	d := layoutDriver(t, "")
	leftW, _, _ := d.m.layout()
	if leftW != 30 {
		t.Fatalf("default explorer width = %d", leftW)
	}

	// Grab the explorer's right border and move it 20 columns right.
	d.drag(leftW-1, 10, leftW-1+20, 12)
	if got, _, _ := d.m.layout(); got != 50 {
		t.Fatalf("after drag width = %d, want 50", got)
	}
	d.screen() // every row stays exactly the terminal width

	// Grabbing the other line of the handle works the same way.
	d.drag(50, 5, 40, 5)
	if got, _, _ := d.m.layout(); got != 40 {
		t.Fatalf("after second drag width = %d, want 40", got)
	}

	// Dragging past the edges stops at the minimum sizes.
	d.drag(39, 5, 0, 5)
	if got, _, _ := d.m.layout(); got != minExplorerWidth {
		t.Errorf("min clamp = %d", got)
	}
	d.drag(minExplorerWidth-1, 5, 119, 5)
	if got, _, _ := d.m.layout(); got != 120-minRightWidth {
		t.Errorf("max clamp = %d", got)
	}
	d.screen()

	// A double click resets the split.
	x := 120 - minRightWidth - 1
	d.click(x, 5)
	d.send(tea.MouseReleaseMsg{X: x, Y: 5})
	d.click(x, 5)
	d.send(tea.MouseReleaseMsg{X: x, Y: 5})
	if got, _, _ := d.m.layout(); got != 30 {
		t.Errorf("after double click width = %d", got)
	}
}

func TestDragQueryHandle(t *testing.T) {
	d := layoutDriver(t, "")
	_, topH, _ := d.m.layout()
	if topH != 14 {
		t.Fatalf("default query height = %d", topH)
	}
	d.drag(60, topH, 60, topH+10) // grab the results' top border
	_, topH, bottomH := d.m.layout()
	if topH != 24 || bottomH != 16 {
		t.Fatalf("after drag = %d/%d", topH, bottomH)
	}
	if d.m.query.height != topH-2 || d.m.results.height != bottomH-2 {
		t.Errorf("panes not resized: query %d, results %d", d.m.query.height, d.m.results.height)
	}
	d.drag(60, topH-1, 60, 39)
	if _, topH, _ := d.m.layout(); topH != 40-minResultsHeight {
		t.Errorf("clamp = %d", topH)
	}
	d.screen()
}

func TestHoverHighlightsHandle(t *testing.T) {
	d := layoutDriver(t, "")
	leftW, _, _ := d.m.layout()
	d.send(tea.MouseMotionMsg{X: leftW, Y: 20})
	s := d.screen()
	// Both lines of the handle, minus the corner rows of the right panes.
	if strings.Count(s, "┃") < 38+38-4 {
		t.Errorf("explorer handle not highlighted:\n%s", s)
	}
	d.send(tea.MouseMotionMsg{X: 80, Y: 20})
	if s := d.screen(); strings.Contains(s, "┃") || strings.Contains(s, "━") {
		t.Errorf("highlight stayed after leaving the handle")
	}
	_, topH, _ := d.m.layout()
	d.send(tea.MouseMotionMsg{X: 80, Y: topH - 1})
	if s := d.screen(); !strings.Contains(s, "━━━━") {
		t.Errorf("query handle not highlighted")
	}
}

func TestKeyboardResizeAndPersist(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	d := layoutDriver(t, sessionPath)
	d.key("alt+shift+right", "alt+shift+right", "alt+shift+down")
	leftW, topH, _ := d.m.layout()
	if leftW != 34 || topH != 15 {
		t.Fatalf("after keys = %d/%d", leftW, topH)
	}

	// The layout comes back on the next start, at the same proportions.
	d.m.Close()
	d2 := layoutDriver(t, sessionPath)
	if l, q, _ := d2.m.layout(); l != leftW || q != topH {
		t.Errorf("restored = %d/%d, want %d/%d", l, q, leftW, topH)
	}
	d2.send(tea.WindowSizeMsg{Width: 240, Height: 80})
	if l, q, _ := d2.m.layout(); l != 2*leftW || q != 2*topH {
		t.Errorf("on a larger screen = %d/%d", l, q)
	}

	d2.key("alt+=")
	if l, q, _ := d2.m.layout(); l != 60 || q != 28 {
		t.Errorf("reset = %d/%d", l, q)
	}
}

func TestClickFocusesPane(t *testing.T) {
	d := layoutDriver(t, "")
	for _, c := range []struct {
		x, y int
		want focus
	}{
		{80, 30, focusResults},
		{80, 5, focusQuery},
		{5, 5, focusExplorer},
	} {
		d.click(c.x, c.y)
		d.send(tea.MouseReleaseMsg{X: c.x, Y: c.y})
		if d.m.focus != c.want {
			t.Errorf("click at %d,%d focused %d, want %d", c.x, c.y, d.m.focus, c.want)
		}
	}
	// The editor cursor follows the query pane wherever the border is.
	d.click(80, 5)
	d.drag(29, 10, 49, 10)
	v := d.m.View()
	if v.Cursor == nil || v.Cursor.X != 50+1+5 {
		t.Errorf("cursor = %+v", v.Cursor)
	}
}
