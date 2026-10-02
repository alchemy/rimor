package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"
)

// ── File path prompt ────────────────────────────────────────────────────────

type pathPurpose int

const (
	pathOpen pathPurpose = iota
	pathSaveAs
)

// pathChosenMsg carries a confirmed path for opening or saving a tab.
type pathChosenMsg struct {
	purpose    pathPurpose
	tab        *tab
	path       string
	closeAfter bool // close the tab once saved
}

type closeTabMsg struct{ tab *tab }

const maxMatches = 6

type pathPrompt struct {
	purpose    pathPurpose
	tab        *tab
	closeAfter bool

	input     textinput.Model
	width     int
	matches   []string
	overwrite string // path the user was warned already exists
	status    string
}

func newPathPrompt(purpose pathPurpose, t *tab, closeAfter bool, maxWidth int) (*pathPrompt, tea.Cmd) {
	p := &pathPrompt{purpose: purpose, tab: t, closeAfter: closeAfter, input: newInput()}
	p.setWidth(maxWidth)

	cwd, _ := os.Getwd()
	value := cwd + string(filepath.Separator)
	if purpose == pathSaveAs {
		if t.path != "" {
			value = t.path
		} else {
			value = filepath.Join(cwd, t.name+".sql")
		}
	}
	p.input.SetValue(tildePath(value))
	p.input.CursorEnd()
	p.refreshMatches()
	return p, p.input.Focus()
}

// tildePath shortens paths under the home directory to ~/….
func tildePath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// Paths may use either separator on Windows; os.IsPathSeparator knows
// which ones the platform accepts.
func endsWithSeparator(s string) bool {
	return s != "" && os.IsPathSeparator(s[len(s)-1])
}

func lastSeparator(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if os.IsPathSeparator(s[i]) {
			return i
		}
	}
	return -1
}

func (p *pathPrompt) setWidth(maxWidth int) {
	p.width = min(80, maxWidth)
	p.input.SetWidth(p.width - 6 - 1)
}

// refreshMatches lists directory entries that complete the current value.
func (p *pathPrompt) refreshMatches() {
	value := p.input.Value()
	dir, prefix := filepath.Split(absPath(value))
	if endsWithSeparator(value) {
		dir, prefix = absPath(value), ""
	}
	entries, err := os.ReadDir(dir)
	p.matches = nil
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || (strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".")) {
			continue
		}
		if e.IsDir() {
			name += string(filepath.Separator)
		} else if p.purpose == pathOpen && !strings.EqualFold(filepath.Ext(name), ".sql") && prefix == "" {
			continue // without a prefix, offer SQL files and folders only
		}
		p.matches = append(p.matches, name)
	}
	// Folders first, then files, each alphabetically.
	slices.SortStableFunc(p.matches, func(a, b string) int {
		ad, bd := endsWithSeparator(a), endsWithSeparator(b)
		if ad != bd {
			if ad {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
}

// complete extends the value to the longest prefix shared by the matches.
func (p *pathPrompt) complete() {
	if len(p.matches) == 0 {
		return
	}
	common := p.matches[0]
	for _, m := range p.matches[1:] {
		for !strings.HasPrefix(m, common) {
			common = common[:len(common)-1]
		}
	}
	value := p.input.Value()
	dir := value[:lastSeparator(value)+1]
	p.input.SetValue(dir + common)
	p.input.CursorEnd()
	p.refreshMatches()
}

func (p *pathPrompt) submit() tea.Cmd {
	path := absPath(strings.TrimSpace(p.input.Value()))
	info, err := os.Stat(path)

	switch p.purpose {
	case pathOpen:
		switch {
		case err != nil:
			p.status = errorStyle.Render("No such file.")
			return nil
		case info.IsDir():
			p.input.SetValue(tildePath(path) + string(filepath.Separator))
			p.input.CursorEnd()
			p.refreshMatches()
			return nil
		}
	case pathSaveAs:
		if err == nil && info.IsDir() {
			p.status = errorStyle.Render("That is a folder; add a file name.")
			return nil
		}
		if filepath.Ext(path) == "" {
			path += ".sql"
			info, err = os.Stat(path)
		}
		if dir, derr := os.Stat(filepath.Dir(path)); derr != nil || !dir.IsDir() {
			p.status = errorStyle.Render("The folder does not exist.")
			return nil
		}
		if err == nil && path != p.tab.path && p.overwrite != path {
			p.overwrite = path
			p.status = lipgloss.NewStyle().Foreground(colorYellow).
				Render(filepath.Base(path) + " exists. Press enter again to replace it.")
			return nil
		}
	}
	msg := pathChosenMsg{purpose: p.purpose, tab: p.tab, path: path, closeAfter: p.closeAfter}
	return func() tea.Msg { return msg }
}

func (p *pathPrompt) Update(msg tea.Msg) (modal, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			return p, closeModal
		case "enter":
			return p, p.submit()
		case "tab":
			p.complete()
			return p, nil
		}
	}
	before := p.input.Value()
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	if p.input.Value() != before {
		p.status, p.overwrite = "", ""
		p.refreshMatches()
	}
	return p, cmd
}

func (p *pathPrompt) View(int) string {
	inner := p.width - 6
	rows := []string{"", p.input.View(), ""}
	if p.status != "" {
		rows = append(rows, wrapLines(p.status, inner, 2)...)
	} else {
		shown := p.matches[:min(len(p.matches), maxMatches)]
		for _, m := range shown {
			st := mutedStyle
			if endsWithSeparator(m) {
				st = titleStyle
			}
			rows = append(rows, st.Render(m))
		}
		if extra := len(p.matches) - len(shown); extra > 0 {
			rows = append(rows, mutedStyle.Italic(true).Render("…and "+strconv.Itoa(extra)+" more"))
		}
		if len(p.matches) == 0 {
			rows = append(rows, mutedStyle.Italic(true).Render("no matches"))
		}
	}
	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))

	title, action := "Open file", "open"
	if p.purpose == pathSaveAs {
		title, action = "Save "+p.tab.title()+" as", "save"
	}
	pn := pane{title: title, footer: hints("tab", "complete", "⏎", action, "esc", "cancel")}
	return pn.render(body, p.width, lipgloss.Height(body)+3, true)
}

// ── Close a tab with unsaved changes ────────────────────────────────────────

type confirmCloseTab struct{ tab *tab }

func (c confirmCloseTab) Update(msg tea.Msg) (modal, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return c, nil
	}
	t := c.tab
	switch key.String() {
	case "s", "enter":
		if t.path != "" {
			return c, func() tea.Msg { return pathChosenMsg{pathSaveAs, t, t.path, true} }
		}
		return c, func() tea.Msg { return promptPathMsg{purpose: pathSaveAs, tab: t, closeAfter: true} }
	case "d":
		return c, func() tea.Msg { return closeTabMsg{t} }
	case "esc", "n", "q":
		return c, closeModal
	}
	return c, nil
}

func (c confirmCloseTab) View(maxWidth int) string {
	width := min(52, maxWidth)
	body := lipgloss.NewStyle().Padding(1, 2).Width(width - 2).Render(
		accentStyle.Bold(true).Render(c.tab.title()) + textStyle.Render(" has unsaved changes."))
	p := pane{title: "Close tab", footer: hints("s", "save", "d", "discard", "esc", "keep")}
	return p.render(body, width, lipgloss.Height(body)+2, true)
}

// ── Context picker ──────────────────────────────────────────────────────────

type contextChosenMsg struct {
	tab *tab
	ctx queryContext
}

// expandConnMsg asks the explorer to open a connection so its databases
// can be listed.
type expandConnMsg struct{ conn *connection }

type pickerItem struct {
	ctx    queryContext
	detail string
	state  connState // for connection rows
}

type contextPicker struct {
	tab    *tab
	filter textinput.Model
	items  []pickerItem
	cursor int
	offset int // first visible item
	rows   int // list rows that fit on screen
	width  int
}

// pickerChrome is the rows around the list: borders, filter, spacing.
const pickerChrome = 6

func newContextPicker(t *tab, maxWidth int) (*contextPicker, tea.Cmd) {
	p := &contextPicker{tab: t, filter: newInput()}
	p.filter.Placeholder = "type to filter"
	p.setWidth(maxWidth)
	return p, p.filter.Focus()
}

func (p *contextPicker) setWidth(maxWidth int) {
	p.width = min(60, maxWidth)
	p.filter.SetWidth(p.width - 6 - 3)
}

func (p *contextPicker) setHeight(maxHeight int) {
	p.rows = max(maxHeight-pickerChrome, 3)
	p.scroll()
}

// window is how many items show at once. Long lists give up two rows to
// the "more above/below" indicators so the dialog keeps its height.
func (p *contextPicker) window() int {
	if p.rows == 0 || len(p.items) <= p.rows {
		return len(p.items)
	}
	return max(p.rows-2, 1)
}

// scroll keeps the selected item inside the window.
func (p *contextPicker) scroll() {
	w := p.window()
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+w {
		p.offset = p.cursor - w + 1
	}
	p.offset = min(max(p.offset, 0), max(len(p.items)-w, 0))
}

func (p *contextPicker) move(delta int) {
	p.cursor = min(max(p.cursor+delta, 0), max(len(p.items)-1, 0))
	p.scroll()
}

// refresh rebuilds the list from the explorer, keeping the selection.
func (p *contextPicker) refresh(e *Explorer) {
	var selected *queryContext
	if p.cursor < len(p.items) {
		selected = &p.items[p.cursor].ctx
	} else {
		selected = &p.tab.ctx
	}
	want := *selected

	query := strings.ToLower(strings.TrimSpace(p.filter.Value()))
	matches := func(parts ...string) bool {
		return query == "" || strings.Contains(strings.ToLower(strings.Join(parts, " ")), query)
	}

	items := []pickerItem{}
	if matches("no connection") {
		items = append(items, pickerItem{})
	}
	for _, conn := range e.Connections() {
		name := conn.cfg().Name
		state := e.connState(conn)
		dbs := e.Databases(conn)
		connMatch := matches(name)
		var dbItems []pickerItem
		for _, d := range dbs {
			if matches(name, d.Name) {
				dbItems = append(dbItems, pickerItem{ctx: queryContext{conn, d.Name}, detail: d.Detail})
			}
		}
		if connMatch || len(dbItems) > 0 {
			items = append(items, pickerItem{ctx: queryContext{conn: conn}, state: state})
			items = append(items, dbItems...)
		}
	}
	p.items = items

	p.cursor = 0
	for i, it := range items {
		if it.ctx == want {
			p.cursor = i
		}
	}
	p.scroll()
}

func (p *contextPicker) Update(msg tea.Msg) (modal, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			return p, closeModal
		case "up", "ctrl+p", "ctrl+k":
			p.move(-1)
			return p, nil
		case "down", "ctrl+n", "ctrl+j":
			p.move(1)
			return p, nil
		case "pgup":
			p.move(-p.window())
			return p, nil
		case "pgdown":
			p.move(p.window())
			return p, nil
		case "home":
			p.move(-len(p.items))
			return p, nil
		case "end":
			p.move(len(p.items))
			return p, nil
		case "enter":
			if p.cursor < len(p.items) {
				msg := contextChosenMsg{p.tab, p.items[p.cursor].ctx}
				return p, func() tea.Msg { return msg }
			}
			return p, nil
		case "right", "tab":
			if p.cursor < len(p.items) {
				if conn := p.items[p.cursor].ctx.conn; conn != nil && p.items[p.cursor].ctx.database == "" {
					return p, func() tea.Msg { return expandConnMsg{conn} }
				}
			}
			return p, nil
		}
	}
	var cmd tea.Cmd
	p.filter, cmd = p.filter.Update(msg)
	return p, cmd
}

func (p *contextPicker) View(int) string {
	inner := p.width - 2
	rows := []string{"", "  " + accentStyle.Render("› ") + p.filter.View(), ""}

	w := p.window()
	scrolling := w < len(p.items)
	more := func(n int, arrow string) string {
		if n <= 0 {
			return ""
		}
		return "  " + mutedStyle.Render(arrow+" "+strconv.Itoa(n)+" more")
	}
	if scrolling {
		rows = append(rows, more(p.offset, "↑"))
	}
	for i := p.offset; i < p.offset+w; i++ {
		it := p.items[i]
		selected := i == p.cursor
		st := func(s lipgloss.Style) lipgloss.Style {
			if selected {
				return s.Background(colorCursor)
			}
			return s
		}
		plain := lipgloss.NewStyle()
		gap := st(plain).Render(" ")

		var s string
		switch {
		case it.ctx.conn == nil:
			s = st(plain).Render("  ") + st(mutedStyle).Render("○") + gap + st(mutedStyle.Italic(true)).Render("no connection")
		case it.ctx.database == "":
			cfg := it.ctx.conn.cfg()
			ic := driverIcon(cfg.Driver)
			s = st(plain).Render("  ") + st(lipgloss.NewStyle().Foreground(ic.color)).Render(ic.String()) + gap +
				st(textStyle.Bold(true)).Render(cfg.Name) + st(plain).Render("  ")
			switch {
			case it.state.loading:
				s += st(accentStyle).Render("connecting…")
			case it.state.err != nil:
				s += st(errorStyle).Render(firstLine(it.state.err.Error()))
			case cfg.Driver.HasDatabases() && !it.state.loaded:
				s += st(mutedStyle).Render("→ databases")
			default:
				s += st(mutedStyle).Render(cfg.Driver.Short())
			}
		default:
			ic := iconDatabase
			s = st(plain).Render("      ") + st(lipgloss.NewStyle().Foreground(colorLavender)).Render(ic.String()) + gap +
				st(textStyle).Render(it.ctx.database)
			if it.detail != "" {
				s += st(plain).Render("  ") + st(mutedStyle).Render(it.detail)
			}
		}
		if it.ctx == p.tab.ctx {
			s += st(plain).Render("  ") + st(okStyle).Render("✓")
		}
		if pad := inner - lipgloss.Width(s); pad > 0 {
			s += st(plain).Render(strings.Repeat(" ", pad))
		}
		rows = append(rows, s)
	}
	if scrolling {
		rows = append(rows, more(len(p.items)-p.offset-w, "↓"))
	}
	if len(p.items) == 0 {
		rows = append(rows, "  "+mutedStyle.Italic(true).Render("nothing matches"))
	}

	body := strings.Join(rows, "\n")
	pn := pane{title: "Query context", footer: hints("→", "databases", "⏎", "choose", "esc", "cancel")}
	return pn.render(body, p.width, lipgloss.Height(body)+3, true)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// connState is a connection's loading state, as the explorer sees it.
type connState struct {
	loaded, loading bool
	err             error
}

func (e *Explorer) connState(conn *connection) connState {
	r := e.rootOf(conn)
	if r == nil {
		return connState{}
	}
	return connState{loaded: r.loaded, loading: r.loading, err: r.err}
}
