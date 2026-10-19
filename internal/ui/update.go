package ui

import tea "charm.land/bubbletea/v2"

// A newer release (see package update) shows as a marker in the
// explorer's top border for as long as it is newer, and once per release
// as a note under the query pane.

// UpdateMsg says a newer release is out.
type UpdateMsg struct {
	Latest string // its tag, e.g. "v0.7.0"
	URL    string // its page
	// How to update: a command, or "" to point at the release page.
	Command string
	// Announce shows the note: the first time the user hears of it.
	Announce bool
}

// WithUpdate shows what an earlier check found, before the program runs.
func (m Model) WithUpdate(msg UpdateMsg) Model {
	m.showUpdate(msg)
	return m
}

func (m *Model) showUpdate(msg UpdateMsg) {
	m.newRelease = &msg
	if !msg.Announce {
		return
	}
	how := "release notes"
	if msg.URL != "" {
		how = mutedStyle.Hyperlink(msg.URL).Render(how)
	}
	if msg.Command != "" {
		how = mutedStyle.Render(msg.Command)
	}
	m.query.notice = accentStyle.Render("↑ rimor "+msg.Latest+" is out") + mutedStyle.Render(" · ") + how
}

// updateCorner is the explorer's marker, linked to the release.
func (m *Model) updateCorner() string {
	if m.newRelease == nil {
		return ""
	}
	st := accentStyle
	if m.newRelease.URL != "" {
		st = st.Hyperlink(m.newRelease.URL)
	}
	return st.Render("↑ " + m.newRelease.Latest)
}

func (m *Model) updateMsg(msg UpdateMsg) tea.Cmd {
	m.showUpdate(msg)
	return nil
}
