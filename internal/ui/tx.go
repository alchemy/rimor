package ui

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// Transactions left open in a tab. After every run rimor asks the tab's
// session whether it is inside a transaction (see db.Transaction), shows
// it in the status line, and before anything ends the session (closing
// the tab, changing its context, quitting, disconnecting, editing or
// deleting its connection) it asks the user to commit or roll back. Only
// then does the action go ahead.

// txCheckTimeout bounds the check after a run.
const txCheckTimeout = 5 * time.Second

// txCheckedMsg brings a session's transaction state.
type txCheckedMsg struct {
	tab     *tab
	session any // the session asked, to ignore answers about an old one
	state   db.TxState
	err     error
}

// busy reports whether the tab's session is in use: a run, or the check
// that follows it.
func (t *tab) busy() bool { return t.run.busy() || t.txChecking || t.exporting }

// checkTx asks the tab's session about its transaction, once its run is
// over.
func (q *QueryPane) checkTx(t *tab) tea.Cmd {
	if t.session == nil || t.closed || t.sessionCtx.conn == nil {
		t.tx = db.TxState{}
		return nil
	}
	s, d := t.session, t.sessionCtx.conn.cfg().Driver
	t.txChecking = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), txCheckTimeout)
		defer cancel()
		state, err := db.Transaction(ctx, s, d)
		return txCheckedMsg{tab: t, session: s, state: state, err: err}
	}
}

func (q *QueryPane) txChecked(msg txCheckedMsg) {
	t := msg.tab
	t.txChecking = false
	if t.session == nil || any(t.session) != msg.session {
		return // the session went meanwhile
	}
	if msg.err == nil {
		t.tx = msg.state
	}
}

// txMarker is the status line's note of an open transaction.
func txMarker(t *tab) string {
	switch {
	case !t.tx.Open:
		return ""
	case t.tx.Failed:
		return errorStyle.Render("failed transaction: roll back")
	case t.tx.Levels > 1:
		return lipgloss.NewStyle().Foreground(colorPeach).Render(fmt.Sprintf("in transaction (%d levels)", t.tx.Levels))
	}
	return lipgloss.NewStyle().Foreground(colorPeach).Render("in transaction")
}

// Actions that end sessions are asked for with one message and carried
// out with another, once open transactions are dealt with.
type (
	closeTabRequestMsg struct{ tab *tab }
	closeTabNowMsg     struct{ tab *tab }
	applyContextMsg    struct {
		tab *tab
		ctx queryContext
	}
	quitNowMsg       struct{}
	disconnectNowMsg struct{ conn *connection }
	saveConnNowMsg   struct{ msg saveConnectionMsg }
	deleteConnNowMsg struct{ conn *connection }
)

// guardTx carries out then, first asking about the open transactions of
// tabs, whose sessions it will end.
func (m *Model) guardTx(tabs []*tab, then tea.Msg) tea.Cmd {
	var open []*tab
	for _, t := range tabs {
		if t.tx.Open && t.session != nil && !t.busy() {
			open = append(open, t)
		}
	}
	if len(open) == 0 {
		return m.update(then)
	}
	m.modal = &txDialog{tabs: open, then: then, width: min(64, m.dialogWidth())}
	return nil
}

// tabsOn are the tabs whose sessions are on a connection.
func (q *QueryPane) tabsOn(conn *connection) []*tab {
	var out []*tab
	for _, t := range q.tabs {
		if t.sessionCtx.conn == conn {
			out = append(out, t)
		}
	}
	return out
}

// quit leaves rimor once open transactions are dealt with.
func (m *Model) quit() tea.Cmd { return m.guardTx(m.query.tabs, quitNowMsg{}) }

// txDialog asks what to do with open transactions.
type txDialog struct {
	tabs    []*tab
	then    tea.Msg
	width   int
	working string // "committing…" while it runs
	err     string // why the last attempt failed
}

// txEndedMsg reports how ending the transactions went.
type txEndedMsg struct {
	dialog *txDialog
	errs   []error // per tab
}

func (d *txDialog) setWidth(maxWidth int) { d.width = min(64, maxWidth) }

func (d *txDialog) anyFailed() bool {
	for _, t := range d.tabs {
		if t.tx.Failed {
			return true
		}
	}
	return false
}

func (d *txDialog) Update(msg tea.Msg) (modal, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok || d.working != "" {
		return d, nil
	}
	switch key.String() {
	case "c":
		if d.anyFailed() {
			d.err = "A failed transaction cannot be committed: roll it back."
			return d, nil
		}
		return d, d.end(true)
	case "r":
		return d, d.end(false)
	case "esc":
		return d, closeModal
	}
	return d, nil
}

// end commits or rolls back every tab's transaction, one after the other.
func (d *txDialog) end(commit bool) tea.Cmd {
	d.working, d.err = "Rolling back…", ""
	if commit {
		d.working = "Committing…"
	}
	type job struct {
		s      *sql.Conn
		driver db.Driver
	}
	var jobs []job
	for _, t := range d.tabs {
		jobs = append(jobs, job{t.session, t.sessionCtx.conn.cfg().Driver})
	}
	return func() tea.Msg {
		errs := make([]error, len(jobs))
		for i, j := range jobs {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			errs[i] = db.EndTransaction(ctx, j.s, j.driver, commit)
			cancel()
		}
		return txEndedMsg{dialog: d, errs: errs}
	}
}

// txEnded goes ahead with the action once every transaction ended. A
// session whose connection is gone has nothing left to end: the server
// rolled it back.
func (m *Model) txEnded(msg txEndedMsg) tea.Cmd {
	d := msg.dialog
	d.working = ""
	var failed []string
	for i, err := range msg.errs {
		t := d.tabs[i]
		if err == nil || db.IsConnLost(err) {
			t.tx = db.TxState{}
			continue
		}
		failed = append(failed, t.title()+": "+db.Describe(err).Message)
	}
	if len(failed) > 0 {
		d.err = strings.Join(failed, "\n")
		return nil
	}
	if m.modal == d {
		m.modal = nil
	}
	return m.update(d.then)
}

func (d *txDialog) View(int) string {
	inner := d.width - 6
	var rows []string
	what := "This tab has a transaction that is not committed:"
	if len(d.tabs) > 1 {
		what = fmt.Sprintf("These %d tabs have transactions that are not committed:", len(d.tabs))
	}
	rows = append(rows, "", textStyle.Render(what), "")
	for _, t := range d.tabs {
		line := accentStyle.Bold(true).Render(t.title()) + mutedStyle.Render("  "+contextLabel(t.sessionCtx))
		if note := txMarker(t); note != "" {
			line += mutedStyle.Render(" · ") + note
		}
		rows = append(rows, "  "+line)
	}
	rows = append(rows, "")
	switch {
	case d.working != "":
		rows = append(rows, accentStyle.Render(d.working))
	case d.err != "":
		for _, l := range wrapLines(d.err, inner, 6) {
			rows = append(rows, errorStyle.Render(l))
		}
	default:
		rows = append(rows, mutedStyle.Render("Closing the session without either rolls it back."))
	}
	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))
	footer := hints("c", "commit", "r", "roll back", "esc", "cancel")
	if d.anyFailed() {
		footer = hints("r", "roll back", "esc", "cancel")
	}
	p := pane{title: "Open transaction", footer: footer}
	return p.render(body, d.width, lipgloss.Height(body)+3, true)
}

// contextLabel is a context as plain text: "ERP › sales".
func contextLabel(c queryContext) string {
	if c.conn == nil {
		return ""
	}
	if c.database != "" {
		return c.conn.cfg().Name + " › " + c.database
	}
	return c.conn.cfg().Name
}

// errTxLost explains a lost connection inside a transaction.
var errTxLost = errors.New("the connection was lost inside a transaction: the server rolled it back, and the statement did not run")
