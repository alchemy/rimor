package editor

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// ctrl+v pastes the system clipboard, which a terminal program can only
// ask the terminal for (OSC 52). Some terminals answer at once, some ask
// the user first (kitty and Ghostty by default), many never answer. So the
// editor asks, waits, and falls back to what it copied itself; a terminal
// that stayed silent is not asked again, and one that answered is given
// longer, since it may be waiting for the user.

// Waits for the terminal's answer.
const (
	firstAnswerWait = 1500 * time.Millisecond // not known to answer yet
	answerWait      = 15 * time.Second        // known to answer, maybe after asking
)

// What the terminal did with earlier requests, for the whole program.
var (
	terminalAnswers bool // it answered once: wait for it
	terminalSilent  bool // it never answered: do not ask
)

// PasteTimeoutMsg ends a wait for the terminal's clipboard.
type PasteTimeoutMsg struct{ seq int }

// paste starts a ctrl+v.
func (m *Model) paste() tea.Cmd {
	if terminalSilent {
		m.pasteText(m.clipboard)
		return nil
	}
	m.pasteSeq++
	m.pasting = m.pasteSeq
	seq, wait := m.pasteSeq, firstAnswerWait
	if terminalAnswers {
		wait = answerWait
	}
	return tea.Batch(tea.ReadClipboard, tea.Tick(wait, func(time.Time) tea.Msg { return PasteTimeoutMsg{seq} }))
}

// clipboardAnswered pastes the terminal's answer, if a paste waits for it;
// an empty answer (nothing there, or refused) falls back to the editor's
// own copy.
func (m *Model) clipboardAnswered(msg tea.ClipboardMsg) {
	if msg.Selection != 0 && msg.Selection != 'c' {
		return // the primary selection, not asked for
	}
	terminalAnswers, terminalSilent = true, false
	if m.pasting == 0 {
		return // answered after the wait: next time it is waited for
	}
	m.pasting = 0
	if msg.Content != "" {
		m.pasteText(msg.Content)
	} else {
		m.pasteText(m.clipboard)
	}
}

func (m *Model) pasteTimedOut(msg PasteTimeoutMsg) {
	if m.pasting == 0 || msg.seq != m.pasting {
		return
	}
	m.pasting = 0
	if !terminalAnswers {
		terminalSilent = true
	}
	m.pasteText(m.clipboard)
}

func (m *Model) pasteText(s string) {
	if s == "" || m.ReadOnly {
		return
	}
	m.checkpoint(editOther)
	m.insert(s)
}
