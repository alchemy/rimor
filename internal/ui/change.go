package ui

import (
	"errors"
	"strings"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// Change tabs: the explorer's "change" script of a procedure, function,
// view or trigger opens its definition in a tab that remembers the object
// (tab.change). Running the whole tab applies it through db.ApplyChange,
// which first checks that nobody changed the object since; if somebody
// did, a dialog asks before overwriting. Once the run is over the tab
// reads the definition again, so its next run compares with what it made.
// A selection runs as plain SQL.

// changeForceMsg runs a change tab over someone else's change.
type changeForceMsg struct{ tab *tab }

// cannotChange is why running the tab cannot apply its change now, or "".
func (t *tab) cannotChange() string {
	cfg := t.ctx.conn.cfg()
	what := db.KindName(t.change.Kind)
	switch {
	case cfg.ReadOnly:
		return "The connection is read-only: the " + what + " cannot be changed"
	case cfg.Driver == db.MySQL && t.tx.Open && t.session != nil:
		return "Commit or roll back first: changing a " + what + " commits the open transaction"
	}
	return ""
}

// changeMarker is the status line's note of a change tab.
func changeMarker(t *tab) string {
	if t.change == nil {
		return ""
	}
	return lipgloss.NewStyle().Foreground(colorPeach).Render("changes " + t.change.What())
}

// changeConflict opens the dialog when a change was refused because the
// object changed since the tab opened it.
func (m *Model) changeConflict(msg runDoneMsg) {
	var changed *db.ChangedError
	if !errors.As(msg.err, &changed) || msg.tab.closed || msg.tab.change == nil {
		return
	}
	m.modal = &changeDialog{tab: msg.tab, err: changed, width: min(64, m.dialogWidth())}
}

// changed records what the object became after a change run, and says so
// when the run went well.
func (q *QueryPane) changed(t *tab, definition string, err error) {
	if t.change == nil || t.run == nil || !t.run.change {
		return
	}
	if err == nil {
		t.change.Original = definition
	}
	if !runFailed(t.run) {
		q.notice = okStyle.Render("✓ Changed the " + t.change.What())
	}
}

// runFailed reports an error in a run, or in any statement of its batch.
func runFailed(r *run) bool {
	return r.err != nil || r.res != nil && r.res.Batch != nil && r.res.Batch.Err() != nil
}

// changeDialog asks whether to overwrite someone else's change.
type changeDialog struct {
	tab   *tab
	err   *db.ChangedError
	width int
}

func (d *changeDialog) setWidth(maxWidth int) { d.width = min(64, maxWidth) }

func (d *changeDialog) Update(msg tea.Msg) (modal, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return d, nil
	}
	switch key.String() {
	case "o":
		t := d.tab
		return nil, func() tea.Msg { return changeForceMsg{t} }
	case "esc":
		return d, closeModal
	}
	return d, nil
}

func (d *changeDialog) View(int) string {
	inner := d.width - 6
	what := d.err.What
	text := "The " + what + " was changed by someone else since this tab opened it. Running the tab now would overwrite their change."
	action := "overwrite it"
	if d.err.Gone {
		text = "The " + what + " was dropped since this tab opened it."
		action = "create it again"
	}
	rows := []string{""}
	for _, l := range wrapLines(text, inner, 6) {
		rows = append(rows, textStyle.Render(l))
	}
	rows = append(rows, "", mutedStyle.Render("To see theirs first, open the "+db.KindName(d.tab.change.Kind)+"'s change script again."))
	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))
	p := pane{title: "Changed meanwhile", footer: hints("o", action, "esc", "cancel")}
	return p.render(body, d.width, lipgloss.Height(body)+3, true)
}
