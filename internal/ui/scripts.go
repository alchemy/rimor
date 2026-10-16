package ui

import (
	"context"
	"strings"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// Scripts from the explorer (s, or S for the first one): ready-made SQL
// for the object under the cursor, opened in a new tab and never run.

type (
	// scriptsReadyMsg brings the scripts of an object, read in the
	// background since they may need its columns.
	scriptsReadyMsg struct {
		object  string
		ctx     queryContext
		scripts []db.Script
		err     error
		first   bool // open the first one rather than offer them
	}
	// openScriptMsg opens a script in a new tab.
	openScriptMsg struct {
		ctx    queryContext
		title  string
		script db.Script
	}
)

// scriptable reports whether the node under the cursor has scripts.
func (e *Explorer) scriptable() bool {
	n := e.cursor
	return n != nil && !n.root && db.HasScripts(n.conn.cfg().Driver, n.obj)
}

// scripts reads the scripts of the node under the cursor.
func (e *Explorer) scripts(first bool) tea.Cmd {
	if !e.scriptable() {
		return nil
	}
	n := e.cursor
	ctx := queryContext{conn: n.conn}
	if d := n.database(); d != nil {
		ctx.database = d.obj.Name
	}
	pool, obj, rows := n.conn.pool, n.obj, e.scriptRows
	name := obj.Name
	if obj.Kind == db.KindColumn || obj.Kind == db.KindForeignKey || obj.Kind == db.KindIndex {
		if obj.Table != "" {
			name = obj.Table // "orders.values" reads better than "status.values"
		}
	}
	return func() tea.Msg {
		c, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		scripts, err := pool.Scripts(c, obj, rows)
		return scriptsReadyMsg{object: name, ctx: ctx, scripts: scripts, err: err, first: first}
	}
}

// scriptsReady offers the scripts, or opens the first.
func (m *Model) scriptsReady(msg scriptsReadyMsg) tea.Cmd {
	if msg.err == nil && msg.first && len(msg.scripts) > 0 {
		m.openScript(openScriptMsg{msg.ctx, msg.object, msg.scripts[0]})
		return nil
	}
	m.modal = &scriptMenu{msg: msg, width: min(60, m.dialogWidth())}
	return nil
}

// openScript puts a script in a new tab, on the object's connection and
// database. The tab starts saved: nothing is lost by closing it.
func (m *Model) openScript(msg openScriptMsg) {
	q := &m.query
	t := q.newTab(msg.ctx)
	t.name = agentTitle(msg.title+"."+msg.script.Name, q.tabs)
	t.ed.SetText(msg.script.SQL)
	t.saved = t.ed.Version()
	if cur := q.current(); cur != nil && cur.path == "" && !cur.dirty() && cur.ed.Text() == "" {
		cur.close()
		q.tabs[q.active] = t
		q.selectTab(q.active)
	} else {
		q.insert(t)
	}
	m.modal = nil
	m.setFocus(focusQuery)
	note := "Opened " + msg.script.Title
	if msg.script.Writes {
		m.query.notice = errorStyle.Render("⚠ "+note+": it writes") + mutedStyle.Render(" · review it, then "+q.runKey)
	} else {
		m.query.notice = mutedStyle.Render(note + " · " + q.runKey + " runs it")
	}
	m.saveSession()
}

// scriptMenu lists an object's scripts.
type scriptMenu struct {
	msg    scriptsReadyMsg
	cursor int
	width  int
}

func (s *scriptMenu) setWidth(maxWidth int) { s.width = min(60, maxWidth) }

func (s *scriptMenu) Update(msg tea.Msg) (modal, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	n := len(s.msg.scripts)
	switch key.String() {
	case "esc", "q":
		return s, closeModal
	case "up", "k":
		if n > 0 {
			s.cursor = (s.cursor + n - 1) % n
		}
	case "down", "j":
		if n > 0 {
			s.cursor = (s.cursor + 1) % n
		}
	case "home", "g":
		s.cursor = 0
	case "end", "G":
		s.cursor = max(n-1, 0)
	case "enter":
		if n == 0 {
			return s, closeModal
		}
		open := openScriptMsg{s.msg.ctx, s.msg.object, s.msg.scripts[s.cursor]}
		return s, func() tea.Msg { return open }
	}
	return s, nil
}

func (s *scriptMenu) View(int) string {
	inner := s.width - 2
	rows := []string{""}
	if s.msg.err != nil {
		for _, l := range wrapLines(s.msg.err.Error(), inner-4, 6) {
			rows = append(rows, "  "+errorStyle.Render(l))
		}
	}
	for i, sc := range s.msg.scripts {
		selected := i == s.cursor
		st := func(x lipgloss.Style) lipgloss.Style {
			if selected {
				return onCursor(x)
			}
			return x
		}
		plain := lipgloss.NewStyle()
		mark := st(plain).Render("  ")
		if selected {
			mark = st(accentStyle).Render("› ")
		}
		line := st(plain).Render(" ") + mark + st(textStyle).Render(sc.Title)
		if sc.Writes {
			line += st(plain).Render("  ") + st(mutedStyle).Render("writes")
		}
		if pad := inner - lipgloss.Width(line); pad > 0 {
			line += st(plain).Render(strings.Repeat(" ", pad))
		}
		rows = append(rows, line)
	}
	rows = append(rows, "")
	body := strings.Join(rows, "\n")
	footer := hints("⏎", "open", "esc", "cancel")
	if s.msg.err != nil {
		footer = hints("esc", "close")
	}
	p := pane{title: "Scripts · " + s.msg.object, footer: footer}
	return p.render(body, s.width, lipgloss.Height(body)+2, true)
}
