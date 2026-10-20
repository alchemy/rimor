package ui

import (
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// A one-shot session: rimor started on a connection from the command line
// (rimor postgres://me@host/erp). The connection is first in the explorer
// and the one tab runs on it; nothing about it is saved, and the session
// file is neither read nor written, so the usual tabs are there next time.

// WithTemporary adds the command line's connection, opens it, and points
// the tab at it.
func (m Model) WithTemporary(cfg db.Config) Model {
	conn, cmd := m.explorer.AddTemporary(cfg)
	conn.askPassword = !db.HasPassword(cfg.Driver, cfg.DSN) && cfg.Driver != db.SQLite
	conn.bareDSN = cfg.DSN
	if t := m.query.current(); t != nil {
		t.setContext(queryContext{conn: conn})
	}
	m.setFocus(focusQuery)
	m.startup = append(m.startup, cmd)
	return m
}

// askPasswordIfRefused opens the password dialog when the server refused
// a temporary connection that came without a password.
func (m *Model) askPasswordIfRefused(msg loadedMsg) tea.Cmd {
	n := msg.node
	if !n.root || !n.conn.temporary || !n.conn.askPassword || !db.IsAuthError(msg.err) || m.modal != nil {
		return nil
	}
	p := newPasswordPrompt(n.conn, m.dialogWidth())
	if db.HasPassword(n.conn.cfg().Driver, n.conn.cfg().DSN) {
		p.note = "That password was refused."
	}
	m.modal = p
	return p.input.Focus()
}

// passwordChosenMsg brings the password typed for a temporary connection.
type passwordChosenMsg struct {
	conn     *connection
	password string
}

// usePassword connects again with the password, which stays in memory.
func (m *Model) usePassword(msg passwordChosenMsg) tea.Cmd {
	m.modal = nil
	conn := msg.conn
	cfg := conn.cfg()
	cfg.DSN = db.WithPassword(cfg.Driver, conn.bareDSN, msg.password)
	m.query.StopConnection(conn)
	conn.setConfig(cfg)
	if r := m.explorer.rootOf(conn); r != nil {
		r.reset()
		return m.explorer.load(r)
	}
	return nil
}

// passwordPrompt asks for a temporary connection's password.
type passwordPrompt struct {
	conn  *connection
	input textinput.Model
	note  string
	width int
}

func newPasswordPrompt(conn *connection, maxWidth int) *passwordPrompt {
	p := &passwordPrompt{conn: conn, input: newInput()}
	p.input.EchoMode = textinput.EchoPassword
	p.input.EchoCharacter = '•'
	p.setWidth(maxWidth)
	return p
}

func (p *passwordPrompt) setWidth(maxWidth int) {
	p.width = min(60, maxWidth)
	p.input.SetWidth(p.width - 8)
}

func (p *passwordPrompt) Update(msg tea.Msg) (modal, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			return p, closeModal
		case "enter":
			chosen := passwordChosenMsg{p.conn, p.input.Value()}
			return p, func() tea.Msg { return chosen }
		}
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return p, cmd
}

func (p *passwordPrompt) View(int) string {
	body := lipgloss.NewStyle().Padding(1, 2).Render(
		textStyle.Render("The server wants a password for ") + accentStyle.Bold(true).Render(p.conn.cfg().Name) + "\n\n" +
			accentStyle.Render("› ") + p.input.View() + "\n\n" +
			noteLine(p.note))
	pn := pane{title: "Password", footer: hints("⏎", "connect", "esc", "cancel")}
	return pn.render(body, p.width, lipgloss.Height(body)+2, true)
}

func noteLine(note string) string {
	if note != "" {
		return errorStyle.Render(note)
	}
	return mutedStyle.Render("Kept in memory only, for this session.")
}
