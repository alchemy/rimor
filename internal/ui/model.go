package ui

import (
	"errors"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

type focus int

const (
	focusExplorer focus = iota
	focusQuery
	focusResults
	focusCount
)

// Layout proportions, in percent of the terminal.
const (
	explorerWidthPct = 25
	queryHeightPct   = 35
)

const (
	minWidth  = 40
	minHeight = 10
)

type Model struct {
	width, height int
	focus         focus
	panes         [focusCount]pane

	explorer    Explorer
	query       QueryPane
	results     ResultsPane
	sessionPath string // "" disables the session file
	modal       modal  // nil when no dialog is open
	split       layout

	keys         keymap
	help         *helpOverlay // the key help, over everything; nil when closed
	waiters      []*runWaiter // agents waiting for a tab's run to end
	leader       bool         // the leader key was pressed; the next key picks an action
	leaderRepeat bool         // a resize step ran; further steps need no new leader

	// theme is the resolved theme setting; the palette may still adapt to
	// the terminal's background when it reports it.
	theme themeChoice

	// disambiguated is set once the terminal confirms the kitty keyboard
	// protocol, so keys such as ctrl+enter can be told apart and shown in
	// hints; until then hints use keys every terminal reports.
	disambiguated bool
}

// New builds the UI from settings, saved connections and the last session.
func New(store db.Store, sessionPath string, settings config.Settings) Model {
	theme := chooseTheme(settings.Theme)
	applyPalette(theme.palette)
	setIcons(settings.Icons)
	keys := newKeymap(settings)
	m := Model{
		panes: [focusCount]pane{
			focusExplorer: {index: 1, title: "Explorer"},
			focusQuery:    {index: 2, title: "Query"},
			focusResults: {index: 3, title: "Results",
				footer: hints(helpKey(keys.first("help", false)), "keys", shortKey(keys.first("quit", false)), "quit")},
		},
		keys:        keys,
		theme:       theme,
		explorer:    NewExplorer(store),
		sessionPath: sessionPath,
		split:       defaultLayout(),
	}
	m.updateRunHint()
	limit := int64(settings.ResultMemoryMB) << 20
	m.query.memoryLimit, m.results.memoryLimit = limit, limit
	m.query.spinner = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle()))
	if sessionPath != "" {
		s, err := loadSession(sessionPath)
		if err != nil {
			m.query.notice = errorStyle.Render("Could not restore session: " + err.Error())
		}
		m.query.restore(s, m.explorer.ConnectionByName)
		if l := s.Layout; l != nil && l.Explorer > 0 && l.Explorer < 1 && l.Query > 0 && l.Query < 1 {
			m.split.explorerFrac, m.split.queryFrac = l.Explorer, l.Query
		}
	}
	if len(m.query.tabs) == 0 {
		m.query.NewTab(queryContext{})
	}
	return m
}

// Close saves the session and releases open connections.
func (m Model) Close() {
	m.saveSession()
	m.explorer.Close()
}

func (m Model) saveSession() {
	if m.sessionPath != "" {
		s := m.query.snapshot()
		s.Layout = &sessionLayout{Explorer: m.split.explorerFrac, Query: m.split.queryFrac}
		_ = saveSession(m.sessionPath, s)
	}
}

// Init asks the terminal for its background colour, which "auto" uses to
// choose dark or light and the terminal theme to shade the selection.
func (m Model) Init() tea.Cmd { return tea.RequestBackgroundColor }

// setFocus moves the focus. In full screen the newly focused pane takes
// over the screen.
func (m *Model) setFocus(f focus) {
	m.focus = f
	if f == focusQuery {
		m.query.Focus()
	} else {
		m.query.Blur()
	}
}

// updateRunHint shows the first run key this terminal can report.
func (m *Model) updateRunHint() {
	key := keyHint(m.keys.first("run", m.disambiguated))
	m.query.runKey, m.results.runKey = key, key
	m.query.shareKey = keyHint(m.keys.first("agent_results", m.disambiguated))
}

// setFullScreen shows only the focused pane, over the whole screen.
func (m *Model) setFullScreen(on bool) {
	m.split.full = on
	m.split.hover, m.split.drag = handleNone, handleNone
	m.resize()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	if p, ok := m.modal.(*contextPicker); ok {
		p.refresh(&m.explorer)
	}
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		if r, ok := m.modal.(resizer); ok {
			r.setWidth(m.dialogWidth())
		}
		if h, ok := m.modal.(heightLimiter); ok {
			h.setHeight(m.dialogHeight())
		}
		if m.help != nil {
			m.help.setSize(m.dialogWidth(), m.dialogHeight())
		}
		return nil

	// Connections.
	case openFormMsg:
		form, cmd := newConnForm(msg.conn, m.explorer.Names(), m.dialogWidth())
		m.modal = form
		return cmd
	case confirmDeleteMsg:
		m.modal = confirmDelete{msg.conn}
		return nil
	case closeModalMsg:
		m.modal = nil
		return nil
	case saveConnectionMsg:
		m.modal = nil
		if msg.conn != nil {
			m.query.StopConnection(msg.conn) // editing closes its sessions
		}
		m.explorer.Upsert(msg.conn, msg.cfg)
		m.setFocus(focusExplorer)
		return nil
	case deleteConnectionMsg:
		m.modal = nil
		m.query.StopConnection(msg.conn)
		m.explorer.Remove(msg.conn)
		m.query.ForgetConnection(msg.conn)
		m.saveSession()
		return nil

	// Tabs and files.
	case confirmCloseTabMsg:
		m.modal = confirmCloseTab{msg.tab}
		return nil
	case closeTabMsg:
		m.modal = nil
		m.query.Close(msg.tab)
		m.saveSession()
		return nil
	case promptPathMsg:
		p, cmd := newPathPrompt(msg.purpose, msg.tab, msg.closeAfter, m.dialogWidth())
		m.modal = p
		return cmd
	case pathChosenMsg:
		m.modal = nil
		m.choosePath(msg)
		m.saveSession()
		return nil
	case pickContextMsg:
		p, cmd := newContextPicker(msg.tab, m.dialogWidth())
		p.setHeight(m.dialogHeight())
		m.modal = p
		if conn := msg.tab.ctx.conn; conn != nil {
			return tea.Batch(cmd, m.explorer.Expand(conn))
		}
		return cmd
	case expandConnMsg:
		return m.explorer.Expand(msg.conn)
	case contextChosenMsg:
		m.modal = nil
		msg.tab.setContext(msg.ctx)
		m.saveSession()
		return nil

	case AgentRequestMsg:
		return m.agentRequest(msg)

	case runDoneMsg, rowsMsg:
		cmd := m.query.Update(msg)
		m.checkWaiters()
		return cmd
	case waitTimeoutMsg:
		m.waitTimedOut(msg.w)
		return nil

	// The cell popup.
	case openCellMsg:
		if msg.tab.run == nil || msg.tab.run.res == nil || !msg.tab.run.res.HasRows() {
			return nil
		}
		p := newCellPopup(msg.tab, msg.row, msg.col, m.dialogWidth())
		p.setHeight(m.dialogHeight())
		m.modal = p
		return m.prepareCell(p)
	case cellReadyMsg:
		if r := msg.popup.run; r.origin == nil && r.originErr == nil {
			r.origin, r.originErr = msg.origin, msg.originErr // once per result
		}
		if m.modal == msg.popup {
			msg.popup.ready(msg)
		}
		return nil
	case cellSavedMsg:
		p := msg.popup
		if msg.err != nil {
			p.phase = cellEditing
			p.err = reasonFor(msg.err)
			if errors.Is(msg.err, db.ErrRowChanged) {
				p.phase, p.reason, p.err = cellViewing, reasonFor(msg.err), ""
				p.ed.ReadOnly = true
			}
			return nil
		}
		p.run.res.Rows.SetCell(p.row, p.col, msg.value)
		m.results.notice = okStyle.Render("✓ saved " + p.column.Name)
		if m.modal == p {
			m.modal = nil
		}
		return nil
	case disconnectMsg:
		m.query.StopConnection(msg.conn)
		m.explorer.disconnect(msg.conn)
		return nil
	case tea.FocusMsg:
		// The terminal's theme may have changed while another window had
		// focus (on Omarchy, a system theme switch): ask for the background
		// again, which re-picks dark or light and re-shades the cursor row.
		return tea.RequestBackgroundColor
	case tea.BackgroundColorMsg:
		if p := m.theme.forBackground(msg.Color, msg.IsDark()); p != current {
			applyPalette(p)
			m.query.SetTheme(editorTheme)
		}
		return nil
	case tea.KeyboardEnhancementsMsg:
		m.disambiguated = msg.SupportsKeyDisambiguation()
		m.updateRunHint()
		return nil
	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		if m.modal != nil {
			var cmd tea.Cmd
			m.modal, cmd = m.modal.Update(msg)
			return cmd
		}
		if m.focus == focusQuery {
			return m.query.Update(msg)
		}
		return nil
	}

	// Background messages: loads, spinner ticks, cursor blinks.
	cmds := []tea.Cmd{m.explorer.Update(msg)}
	if _, ok := msg.(spinner.TickMsg); ok {
		cmds = append(cmds, m.query.Update(msg))
	}
	if m.modal != nil {
		var cmd tea.Cmd
		m.modal, cmd = m.modal.Update(msg)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	// After the leader key, the next key picks an action; anything else
	// (esc included) just ends the sequence.
	if m.leader {
		action, ok := m.keys.chord[key]
		m.leader = ok && repeatable[action]
		m.leaderRepeat = m.leader
		if ok {
			return m.do(action)
		}
		return nil
	}

	action, bound := m.keys.direct[key]
	if bound && action == "quit" {
		return tea.Quit
	}
	// The help overlay takes every key while open; the help key opens it
	// from anywhere, a dialog included.
	if m.help != nil {
		if m.help.Update(msg) || (bound && action == "help") {
			m.help = nil
		}
		return nil
	}
	if bound && action == "help" {
		return m.do(action)
	}
	if m.modal != nil {
		var cmd tea.Cmd
		m.modal, cmd = m.modal.Update(msg)
		return cmd
	}

	// App-wide shortcuts work everywhere, including in the editor.
	if key == m.keys.leader {
		m.leader, m.leaderRepeat = true, false
		return nil
	}
	if bound {
		return m.do(action)
	}

	if m.focus == focusQuery {
		return m.query.Update(msg)
	}

	// Panes without text input keep single-key shortcuts.
	switch key {
	case "q", "ctrl+c":
		return tea.Quit
	case "tab":
		m.setFocus((m.focus + 1) % focusCount)
		return nil
	case "shift+tab":
		m.setFocus((m.focus + focusCount - 1) % focusCount)
		return nil
	case "1", "2", "3":
		m.setFocus(focus(key[0] - '1'))
		return nil
	case "?":
		m.help = newHelp(m)
		return nil
	}
	switch m.focus {
	case focusExplorer:
		return m.explorer.Update(msg)
	case focusResults:
		return m.results.Update(msg, m.query.current())
	}
	return nil
}

// choosePath opens or saves a file picked in the path prompt.
func (m *Model) choosePath(msg pathChosenMsg) {
	q := &m.query
	switch msg.purpose {
	case pathOpen:
		if err := q.Open(msg.path, m.explorer.Context()); err != nil {
			q.notice = errorStyle.Render(err.Error())
			return
		}
		m.setFocus(focusQuery)
	case pathSaveAs:
		if err := q.Save(msg.tab, msg.path); err != nil {
			q.notice = errorStyle.Render(err.Error())
			return
		}
		if msg.closeAfter {
			q.Close(msg.tab)
		}
	}
}

func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeAllMotion
	v.ReportFocus = true // to recheck the background, see FocusMsg
	if p, ok := m.modal.(*cellPopup); ok && m.help == nil && m.width >= minWidth && m.height >= minHeight {
		if x, y, ok := p.cursor(); ok {
			dx, dy := m.dialogPos(p.View(m.dialogWidth()))
			c := tea.NewCursor(dx+x, dy+y)
			c.Shape = tea.CursorBar
			c.Color = colorAccent
			v.Cursor = c
		}
	}
	if m.modal == nil && m.help == nil && m.width >= minWidth && m.height >= minHeight {
		if x, y, ok := m.query.Cursor(); ok {
			left := 0 // the query pane's left edge
			if !m.split.full {
				left, _, _ = m.layout()
			}
			c := tea.NewCursor(left+1+x, 1+y)
			c.Shape = tea.CursorBar
			c.Color = colorAccent
			v.Cursor = c
		}
	}
	return v
}

// renderPane draws one pane at the given size, with its borders, labels
// and any highlighted resize handles.
func (m Model) renderPane(f focus, width, height int) string {
	// Panes lose focus styling while a dialog is open, so the dialog stands out.
	focused := m.modal == nil && m.focus == f
	hot := m.split.hot()
	p := m.panes[f]
	// The leader indicator goes on the query pane, which is always wide
	// enough, or on the only pane in full screen.
	indicator := m.split.full || f == focusQuery
	switch {
	case indicator && m.leader && m.leaderRepeat:
		p.corner = accentStyle.Render("resizing") + mutedStyle.Render(" · any key ends")
	case indicator && m.leader:
		p.corner = accentStyle.Render(m.keys.leader + " …")
	case m.split.full:
		if key := m.keys.first("full_screen", m.disambiguated); key != "" {
			p.corner = hints(key, "exit full screen")
		} else {
			p.corner = mutedStyle.Render("full screen")
		}
	}

	switch f {
	case focusExplorer:
		p.hotRight = hot == handleExplorer
		if focused {
			p.footer = m.explorer.Hints()
		}
		return p.render(m.explorer.View(focused), width, height, focused)

	case focusQuery:
		p.hotLeft, p.hotBottom = hot == handleExplorer, hot == handleQuery
		p.label = m.query.Tabs(width-2-8-lipgloss.Width(p.corner), focused)
		p.status = m.query.Status()
		p.footer = m.query.Footer(focused)
		return p.render(m.query.View(), width, height, focused)
	}

	p.hotLeft, p.hotTop = hot == handleExplorer, hot == handleQuery
	cur, spin := m.query.current(), m.query.spinner.View()
	p.status = m.results.Status(cur, spin)
	if foot := m.results.Footer(cur, focused); foot != "" {
		p.footer = foot
	}
	return p.render(m.results.View(cur, spin, focused), width, height, focused)
}

// dialogPos is where a dialog sits: centred, a third of the way down.
func (m Model) dialogPos(dialog string) (x, y int) {
	return (m.width - lipgloss.Width(dialog)) / 2, max((m.height-lipgloss.Height(dialog))/3, 0)
}

// dialogWidth and dialogHeight are the largest a dialog may be.
func (m Model) dialogWidth() int  { return m.width - 4 }
func (m Model) dialogHeight() int { return m.height - 2 }

func (m Model) render() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	if m.width < minWidth || m.height < minHeight {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			mutedStyle.Render("terminal too small"))
	}

	var base string
	if m.split.full {
		base = m.renderPane(m.focus, m.width, m.height)
	} else {
		leftW, topH, bottomH := m.layout()
		rightW := m.width - leftW
		right := lipgloss.JoinVertical(lipgloss.Left,
			m.renderPane(focusQuery, rightW, topH),
			m.renderPane(focusResults, rightW, bottomH))
		base = lipgloss.JoinHorizontal(lipgloss.Top, m.renderPane(focusExplorer, leftW, m.height), right)
	}
	// Dialogs, then the key help over everything.
	layers := []*lipgloss.Layer{lipgloss.NewLayer(base)}
	var dialog string
	switch {
	case m.modal != nil:
		dialog = m.modal.View(m.dialogWidth())
	case m.leader && !m.leaderRepeat:
		dialog = m.leaderHelp(m.dialogWidth())
	}
	if dialog != "" {
		x, y := m.dialogPos(dialog)
		layers = append(layers, lipgloss.NewLayer(dialog).X(x).Y(y).Z(1))
	}
	if m.help != nil {
		help := m.help.View()
		x, y := m.dialogPos(help)
		layers = append(layers, lipgloss.NewLayer(help).X(x).Y(min(y, 1)).Z(2))
	}
	if len(layers) == 1 {
		return base
	}
	return lipgloss.NewCompositor(layers...).Render()
}
