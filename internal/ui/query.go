package ui

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
	"rimor.dev/internal/editor"
)

// queryContext is where a tab's SQL runs: a saved connection and,
// optionally, one of its databases ("" is the connection's default).
type queryContext struct {
	conn     *connection
	database string
}

func (c queryContext) language() editor.Language {
	if c.conn == nil {
		return editor.SQL
	}
	switch c.conn.cfg().Driver {
	case db.Postgres:
		return editor.PostgreSQL
	case db.SQLServer:
		return editor.TSQL
	}
	return editor.SQL
}

// render draws the context for the pane's status line:
// "<icon> name › database".
func (c queryContext) render() string {
	if c.conn == nil {
		return mutedStyle.Render("○ no connection")
	}
	cfg := c.conn.cfg()
	ic := driverIcon(cfg.Driver)
	s := lipgloss.NewStyle().Foreground(ic.color).Render(ic.String()) + " " + textStyle.Render(cfg.Name)
	if c.database != "" {
		s += mutedStyle.Render(" › ") + textStyle.Render(c.database)
	}
	return s
}

var editorTheme = editor.Theme{
	Text:              colorText,
	Keyword:           colorAccent,
	Type:              colorYellow,
	Function:          colorBlue,
	String:            colorGreen,
	Number:            colorPeach,
	Comment:           colorMuted,
	Operator:          lipgloss.Color("#89dceb"), // sky
	Punctuation:       colorOverlay,
	Variable:          lipgloss.Color("#eba0ac"), // maroon
	Quoted:            lipgloss.Color("#f5e0dc"), // rosewater
	LineNumber:        lipgloss.Color("#585b70"), // surface2
	CurrentLineNumber: colorAccent,
	Selection:         colorBorder,
	Placeholder:       colorMuted,
}

type tab struct {
	ed    *editor.Model
	path  string // empty until the tab is saved
	name  string // untitled-N, for tabs without a file
	ctx   queryContext
	saved int // editor version at the last load or save

	// Statements run on a connection of their own, opened on first run.
	session    *sql.Conn
	sessionCtx queryContext // context the session belongs to
	run        *run         // the latest run
	runs       int          // generation counter for runs
	closed     bool
}

func (t *tab) title() string {
	if t.path != "" {
		return filepath.Base(t.path)
	}
	return t.name
}

func (t *tab) dirty() bool { return t.ed.Version() != t.saved }

func (t *tab) setContext(ctx queryContext) {
	if ctx == t.ctx {
		return
	}
	t.ctx = ctx
	t.ed.SetLanguage(ctx.language())
	if !t.cancelRun() {
		t.dropSession() // a running statement drops it when it returns
	}
}

// close releases what the tab holds.
func (t *tab) close() {
	t.closed = true
	if !t.cancelRun() {
		t.dropSession()
	}
}

// Messages from the query pane to the model, which owns the dialogs.
type (
	confirmCloseTabMsg struct{ tab *tab }
	promptPathMsg      struct {
		purpose    pathPurpose
		tab        *tab
		closeAfter bool
	}
	pickContextMsg struct{ tab *tab }
)

// QueryPane holds the SQL tabs.
type QueryPane struct {
	tabs     []*tab
	active   int
	untitled int // last untitled-N number handed out

	width, height int // editor area
	focused       bool
	notice        string // shown in the footer until the next key
	spinner       spinner.Model
	runKey        string // the run key for hints, as the terminal reports it
	memoryLimit   int64  // bytes a result's rows may take
}

func (q *QueryPane) current() *tab {
	if len(q.tabs) == 0 {
		return nil
	}
	return q.tabs[q.active]
}

func (q *QueryPane) SetSize(width, height int) {
	q.width, q.height = width, height
	for _, t := range q.tabs {
		t.ed.SetSize(width, height)
	}
}

func (q *QueryPane) Focus() {
	q.focused = true
	if t := q.current(); t != nil {
		t.ed.Focus()
	}
}

func (q *QueryPane) Blur() {
	q.focused = false
	for _, t := range q.tabs {
		t.ed.Blur()
	}
}

// newTab makes a tab without adding it; callers name it or give it a file.
func (q *QueryPane) newTab(ctx queryContext) *tab {
	t := &tab{ed: editor.New(editorTheme)}
	t.ed.Placeholder = "-- write SQL here"
	t.ed.SetSize(q.width, q.height)
	t.setContext(ctx)
	t.saved = t.ed.Version()
	return t
}

// NewTab opens an empty tab after the current one and selects it.
func (q *QueryPane) NewTab(ctx queryContext) {
	t := q.newTab(ctx)
	q.untitled++
	t.name = fmt.Sprintf("untitled-%d", q.untitled)
	q.insert(t)
}

func (q *QueryPane) insert(t *tab) {
	at := min(q.active+1, len(q.tabs))
	q.tabs = append(q.tabs[:at], append([]*tab{t}, q.tabs[at:]...)...)
	q.selectTab(at)
}

func (q *QueryPane) selectTab(i int) {
	if len(q.tabs) == 0 {
		return
	}
	q.active = (i + len(q.tabs)) % len(q.tabs)
	if q.focused {
		q.Focus()
		for j, t := range q.tabs {
			if j != q.active {
				t.ed.Blur()
			}
		}
	}
}

// Close removes a tab without asking.
func (q *QueryPane) Close(t *tab) {
	for i, x := range q.tabs {
		if x == t {
			t.close()
			q.tabs = append(q.tabs[:i], q.tabs[i+1:]...)
			if q.active >= i && q.active > 0 {
				q.active--
			}
			q.selectTab(q.active)
			return
		}
	}
}

// Open loads a file into a new tab, or selects the tab that has it open.
func (q *QueryPane) Open(path string, ctx queryContext) error {
	path = absPath(path)
	for i, t := range q.tabs {
		if t.path == path {
			q.selectTab(i)
			return nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	t := q.newTab(ctx)
	t.path = path
	t.ed.SetText(string(data))
	t.saved = t.ed.Version()

	// An untouched empty tab is replaced rather than kept around.
	if cur := q.current(); cur != nil && cur.path == "" && !cur.dirty() && cur.ed.Text() == "" {
		q.tabs[q.active] = t
		q.selectTab(q.active)
		return nil
	}
	q.insert(t)
	return nil
}

// Save writes a tab to path, which becomes the tab's file.
func (q *QueryPane) Save(t *tab, path string) error {
	path = absPath(path)
	if err := os.WriteFile(path, []byte(t.ed.Text()), 0o644); err != nil {
		return err
	}
	t.path = path
	t.saved = t.ed.Version()
	q.notice = okStyle.Render("Saved " + filepath.Base(path))
	return nil
}

// absPath expands ~ and makes a path absolute.
func absPath(path string) string {
	if path == "~" || (len(path) > 1 && path[0] == '~' && os.IsPathSeparator(path[1])) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// StopConnection stops the statements and fetches running on a
// connection, before it is closed: closing a session waits for its rows.
func (q *QueryPane) StopConnection(conn *connection) {
	for _, t := range q.tabs {
		if t.sessionCtx.conn == conn {
			t.cancelRun()
		}
	}
}

// ForgetConnection clears the context of tabs that used a deleted
// connection.
func (q *QueryPane) ForgetConnection(conn *connection) {
	for _, t := range q.tabs {
		if t.ctx.conn == conn {
			t.setContext(queryContext{})
		}
	}
}

func (q *QueryPane) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case runDoneMsg:
		return q.finish(msg)
	case rowsMsg:
		return q.rowsArrived(msg)
	case spinner.TickMsg:
		if q.running() > 0 {
			var cmd tea.Cmd
			q.spinner, cmd = q.spinner.Update(msg)
			return cmd
		}
		return nil
	}

	t := q.current()
	key, isKey := msg.(tea.KeyPressMsg)
	if isKey {
		q.notice = ""
		switch key.String() {
		case "ctrl+pgdown", "alt+]", "ctrl+tab":
			q.selectTab(q.active + 1)
			return nil
		case "ctrl+pgup", "alt+[", "ctrl+shift+tab":
			q.selectTab(q.active - 1)
			return nil
		}
	}
	if t == nil {
		return nil
	}
	if isKey {
		switch key.String() {
		case "ctrl+w":
			if t.dirty() {
				return func() tea.Msg { return confirmCloseTabMsg{t} }
			}
			q.Close(t)
			return nil
		case "ctrl+s":
			if t.path == "" {
				return func() tea.Msg { return promptPathMsg{purpose: pathSaveAs, tab: t} }
			}
			if err := q.Save(t, t.path); err != nil {
				q.notice = errorStyle.Render(err.Error())
			}
			return nil
		case "alt+s":
			return func() tea.Msg { return promptPathMsg{purpose: pathSaveAs, tab: t} }
		case "ctrl+e":
			return func() tea.Msg { return pickContextMsg{t} }
		case "ctrl+enter", "f5", "alt+enter":
			return q.Run(t)
		case "esc":
			if t.cancelRun() {
				return nil
			}
		}
	}
	return t.ed.Update(msg)
}

// Cursor is the terminal cursor position inside the pane body.
func (q *QueryPane) Cursor() (x, y int, ok bool) {
	if t := q.current(); t != nil && q.focused {
		return t.ed.Cursor()
	}
	return 0, 0, false
}

func (q *QueryPane) View() string {
	t := q.current()
	if t == nil {
		return lipgloss.Place(q.width, q.height, lipgloss.Center, lipgloss.Center,
			mutedStyle.Italic(true).Render("No open queries")+"\n\n"+
				hints("^t", "new tab", "^o", "open file"))
	}
	return t.ed.View()
}

// Tabs renders the tab strip for the pane's top border, scrolled so the
// active tab is visible within width cells.
func (q *QueryPane) Tabs(width int, focused bool) string {
	if len(q.tabs) == 0 {
		return titleStyle.Render("Query")
	}
	border := borderStyle
	if focused {
		border = borderFocusedStyle
	}

	labels := make([]string, len(q.tabs))
	for i, t := range q.tabs {
		dot := mutedStyle.Render("○")
		if t.ctx.conn != nil {
			dot = lipgloss.NewStyle().Foreground(driverColor(t.ctx.conn.cfg().Driver)).Render("●")
		}
		name := mutedStyle.Render(t.title())
		if i == q.active {
			st := textStyle.Bold(true)
			if focused {
				st = accentStyle.Bold(true)
			}
			name = st.Render(t.title())
		}
		label := dot + " " + name
		if t.dirty() {
			label += lipgloss.NewStyle().Foreground(colorPeach).Render(" ●")
		}
		labels[i] = label
	}

	sep := border.Render(" ─ ")
	more := func(s string) string { return mutedStyle.Render(s) }

	// Grow a window around the active tab while it fits.
	from, to := q.active, q.active+1
	used := lipgloss.Width(labels[q.active])
	for grew := true; grew; {
		grew = false
		if to < len(labels) && used+3+lipgloss.Width(labels[to])+4 <= width {
			used += 3 + lipgloss.Width(labels[to])
			to++
			grew = true
		}
		if from > 0 && used+3+lipgloss.Width(labels[from-1])+4 <= width {
			from--
			used += 3 + lipgloss.Width(labels[from])
			grew = true
		}
	}

	s := strings.Join(labels[from:to], sep)
	if from > 0 {
		s = more("‹ ") + s
	}
	if to < len(labels) {
		s += more(" ›")
	}
	return ansi.Truncate(s, width, "…")
}

// Status is the active tab's context, for the bottom border.
func (q *QueryPane) Status() string {
	if t := q.current(); t != nil {
		return t.ctx.render()
	}
	return ""
}

// Footer shows the latest notice, or the cursor position and key hints.
func (q *QueryPane) Footer(focused bool) string {
	if q.notice != "" {
		return q.notice
	}
	t := q.current()
	if t == nil || !focused {
		return ""
	}
	line, col := t.ed.CursorPosition()
	return mutedStyle.Render(fmt.Sprintf("%d:%d", line+1, col+1)) + "  " +
		hints(q.runKey, "run", "^e", "context", "^s", "save")
}
