package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// freshTerminal forgets what earlier tests taught about the terminal.
func freshTerminal(t *testing.T) {
	terminalAnswers, terminalSilent = false, false
	t.Cleanup(func() { terminalAnswers, terminalSilent = false, false })
}

func ctrlV(m *Model) tea.Cmd {
	return m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
}

func TestPasteAsksTheTerminal(t *testing.T) {
	freshTerminal(t)
	m := newModel()
	m.SetText("SELECT ")
	press(m, "end")
	if cmd := ctrlV(m); cmd == nil || m.pasting == 0 {
		t.Fatal("ctrl+v did not ask the terminal")
	}
	if m.Text() != "SELECT " {
		t.Fatal("pasted before the answer")
	}
	m.Update(tea.ClipboardMsg{Content: "1 FROM dual", Selection: 'c'})
	if got := m.Text(); got != "SELECT 1 FROM dual" {
		t.Errorf("text %q", got)
	}
	// The wait's end changes nothing once the answer came.
	m.Update(PasteTimeoutMsg{m.pasteSeq})
	if got := m.Text(); got != "SELECT 1 FROM dual" {
		t.Errorf("after the timeout %q", got)
	}
	// One undo step.
	press(m, "ctrl+z")
	if got := m.Text(); got != "SELECT " {
		t.Errorf("undo %q", got)
	}
	if !terminalAnswers || terminalSilent {
		t.Error("the editor did not learn that the terminal answers")
	}
}

func TestPasteFallsBackToTheEditorsCopy(t *testing.T) {
	freshTerminal(t)
	m := newModel()
	m.SetText("abc")
	press(m, "ctrl+a", "ctrl+c", "end")

	// Refused, or nothing there: the editor's own copy.
	ctrlV(m)
	m.Update(tea.ClipboardMsg{Content: "", Selection: 'c'})
	if got := m.Text(); got != "abcabc" {
		t.Errorf("empty answer: %q", got)
	}

	// A terminal that never answers: the copy after the wait, and later
	// pastes do not ask at all.
	freshTerminal(t)
	ctrlV(m)
	m.Update(PasteTimeoutMsg{m.pasting})
	if got := m.Text(); got != "abcabcabc" || !terminalSilent {
		t.Errorf("timeout: %q, silent %v", got, terminalSilent)
	}
	if cmd := ctrlV(m); cmd != nil || m.Text() != "abcabcabcabc" {
		t.Errorf("a silent terminal was asked again: %q", m.Text())
	}
}

func TestPasteLateAnswer(t *testing.T) {
	freshTerminal(t)
	m := newModel()
	ctrlV(m)
	m.Update(PasteTimeoutMsg{m.pasting}) // waited too little: the user was asked
	m.Update(tea.ClipboardMsg{Content: "late", Selection: 'c'})
	if got := m.Text(); got != "" {
		t.Errorf("a late answer was pasted: %q", got)
	}
	if !terminalAnswers || terminalSilent {
		t.Error("a late answer should make the next wait longer")
	}
	// The primary selection is not what ctrl+v asked for.
	ctrlV(m)
	m.Update(tea.ClipboardMsg{Content: "primary", Selection: 'p'})
	if got := m.Text(); got != "" || m.pasting == 0 {
		t.Errorf("primary selection pasted: %q", got)
	}
}

func TestPasteReadOnly(t *testing.T) {
	freshTerminal(t)
	m := newModel()
	m.ReadOnly = true
	if cmd := ctrlV(m); cmd != nil {
		t.Error("a read-only editor asked for the clipboard")
	}
}
