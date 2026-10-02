package ui

import (
	"strings"
	"testing"
)

func TestFullScreenToggle(t *testing.T) {
	d := layoutDriver(t, "")
	d.drag(29, 10, 39, 10) // a custom layout to come back to
	before, beforeTop, _ := d.m.layout()

	d.key("alt+3", "alt+f")
	if !d.m.split.full || d.m.focus != focusResults {
		t.Fatalf("full=%v focus=%d", d.m.split.full, d.m.focus)
	}
	s := d.screen() // also checks every row is the full width
	if strings.Contains(s, "Explorer") || strings.Contains(s, "untitled-1") {
		t.Errorf("other panes still drawn:\n%s", s)
	}
	if !strings.Contains(s, "Results") || !strings.Contains(s, "exit full screen") {
		t.Errorf("full-screen pane or indicator missing:\n%s", s)
	}
	if d.m.results.width != 118 || d.m.results.height != 38 {
		t.Errorf("results sized %dx%d", d.m.results.width, d.m.results.height)
	}

	// Focus keys move the full screen to another pane.
	d.key("alt+1")
	if s := d.screen(); !d.m.split.full || !strings.Contains(s, "Explorer") || strings.Contains(s, "Results") {
		t.Errorf("full screen did not follow focus:\n%s", s)
	}

	// Resizing does nothing in full screen, and clicks only focus.
	d.key("alt+shift+right")
	d.click(60, 20)
	if d.m.split.drag != handleNone || d.m.focus != focusExplorer {
		t.Errorf("drag=%d focus=%d", d.m.split.drag, d.m.focus)
	}

	d.key("alt+f")
	if l, top, _ := d.m.layout(); d.m.split.full || l != before || top != beforeTop {
		t.Errorf("after exit: full=%v layout %d/%d, want %d/%d", d.m.split.full, l, top, before, beforeTop)
	}
	if d.m.explorer.width != before-2 {
		t.Errorf("explorer not resized back: %d", d.m.explorer.width)
	}
	d.screen()
}

func TestFullScreenPaneKeys(t *testing.T) {
	for key, want := range map[string]focus{
		"alt+shift+1": focusExplorer, // kitty keyboard protocol
		"alt+shift+2": focusQuery,
		"alt+shift+3": focusResults,
		"alt+!":       focusExplorer, // legacy, US layout
		"alt+@":       focusQuery,
		"alt+#":       focusResults,
		`alt+"`:       focusQuery, // legacy, Italian layout
		"alt+£":       focusResults,
	} {
		d := layoutDriver(t, "")
		d.key(key)
		if !d.m.split.full || d.m.focus != want {
			t.Errorf("%s: full=%v focus=%d, want %d", key, d.m.split.full, d.m.focus, want)
		}
		// The keys only enter full screen; alt+f leaves it.
		d.key(key)
		if !d.m.split.full {
			t.Errorf("%s pressed twice left full screen", key)
		}
	}
}

func TestFullScreenEditorCursor(t *testing.T) {
	d := layoutDriver(t, "")
	d.key("alt+shift+2")
	d.typeText("SELECT")
	v := d.m.View()
	// Border, then the 5-cell line-number gutter, then 6 typed characters.
	if v.Cursor == nil || v.Cursor.X != 1+5+6 || v.Cursor.Y != 1 {
		t.Fatalf("cursor = %+v", v.Cursor)
	}
	if d.m.query.width != 118 {
		t.Errorf("editor width %d", d.m.query.width)
	}
	d.key("alt+f")
	leftW, _, _ := d.m.layout()
	if v := d.m.View(); v.Cursor == nil || v.Cursor.X != leftW+1+5+6 {
		t.Errorf("cursor after exit = %+v", v.Cursor)
	}
}
