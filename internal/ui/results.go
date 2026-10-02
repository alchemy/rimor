package ui

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

const maxColumnWidth = 40

// run is one execution of a tab's SQL and what came of it.
type run struct {
	gen     int
	query   string
	line    int // buffer line where the query starts (0-based)
	col     int // buffer column of the query's first character
	started time.Time
	cancel  context.CancelFunc
	running bool

	res *db.Result
	err *db.Error

	grid grid
}

type runDoneMsg struct {
	tab     *tab
	gen     int
	res     *db.Result
	err     error
	session *sql.Conn
}

// Run executes the selection, or the whole tab, on the tab's session.
func (q *QueryPane) Run(t *tab) tea.Cmd {
	if t.run != nil && t.run.running {
		return nil
	}
	if t.ctx.conn == nil {
		q.notice = errorStyle.Render("Choose a connection first (^e)")
		return nil
	}
	text := t.ed.SelectedText()
	line, col := 0, 0
	if text != "" {
		line, col, _ = t.ed.SelectionStart()
	} else {
		text = t.ed.Text()
	}
	if strings.TrimSpace(text) == "" {
		q.notice = mutedStyle.Render("Nothing to run")
		return nil
	}

	if t.session != nil && t.sessionCtx != t.ctx {
		t.dropSession()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.runs++
	t.run = &run{gen: t.runs, query: text, line: line, col: col, started: time.Now(), cancel: cancel, running: true}
	t.sessionCtx = t.ctx

	gen, pool, database, session := t.runs, t.ctx.conn.pool, t.ctx.database, t.session
	exec := func() tea.Msg {
		defer cancel()
		for attempt := 0; ; attempt++ {
			if session == nil {
				s, err := pool.Session(ctx, database)
				if err != nil {
					return runDoneMsg{tab: t, gen: gen, err: err}
				}
				session = s
			}
			res, err := db.Run(ctx, session, text)
			if err != nil && attempt == 0 && db.IsConnLost(err) {
				pool.Release(session) // the connection went away; retry on a fresh one
				session = nil
				continue
			}
			return runDoneMsg{tab: t, gen: gen, res: res, err: err, session: session}
		}
	}

	if q.running() == 1 {
		return tea.Batch(exec, q.spinner.Tick)
	}
	return exec
}

func (q *QueryPane) running() int {
	n := 0
	for _, t := range q.tabs {
		if t.run != nil && t.run.running {
			n++
		}
	}
	return n
}

// finish records the outcome of a run.
func (q *QueryPane) finish(msg runDoneMsg) {
	t := msg.tab
	if msg.session != nil && msg.session != t.session {
		t.dropSession()
		t.session = msg.session
	}
	if t.closed || t.sessionCtx != t.ctx {
		t.dropSession() // the tab went away or moved to another context meanwhile
	}
	r := t.run
	if r == nil || r.gen != msg.gen {
		return
	}
	r.running = false
	r.res = msg.res
	if msg.err != nil {
		e := db.Describe(msg.err)
		r.err = &e
		if db.IsConnLost(msg.err) {
			t.dropSession()
		}
	}
}

// cancelRun stops the tab's running statement, if any.
func (t *tab) cancelRun() bool {
	if t.run != nil && t.run.running {
		t.run.cancel()
		return true
	}
	return false
}

func (t *tab) dropSession() {
	if t.session == nil {
		return
	}
	if t.sessionCtx.conn != nil {
		t.sessionCtx.conn.pool.Release(t.session)
	} else {
		t.session.Close()
	}
	t.session = nil
}

// ── Results pane ────────────────────────────────────────────────────────────

// ResultsPane shows the outcome of the active tab's last run.
type ResultsPane struct {
	width, height int
}

func (rp *ResultsPane) SetSize(width, height int) { rp.width, rp.height = width, height }

func (rp *ResultsPane) Update(msg tea.KeyPressMsg, t *tab) tea.Cmd {
	if t == nil || t.run == nil {
		return nil
	}
	if msg.String() == "esc" && t.cancelRun() {
		return nil
	}
	r := t.run
	if r.res == nil || !r.res.HasRows() {
		return nil
	}
	g, res := &r.grid, r.res
	page := max(rp.height-3, 1)
	switch msg.String() {
	case "up", "k":
		g.row--
	case "down", "j":
		g.row++
	case "left", "h":
		g.col--
	case "right", "l":
		g.col++
	case "pgup", "ctrl+u":
		g.row -= page
	case "pgdown", "ctrl+d":
		g.row += page
	case "g", "ctrl+home":
		g.row = 0
	case "G", "ctrl+end":
		g.row = len(res.Rows) - 1
	case "home", "0":
		g.col = 0
	case "end", "$":
		g.col = len(res.Columns) - 1
	case "y":
		if g.row < len(res.Rows) {
			return tea.SetClipboard(res.Rows[g.row][g.col].Text)
		}
	case "Y":
		if g.row < len(res.Rows) {
			cells := make([]string, len(res.Columns))
			for i, v := range res.Rows[g.row] {
				cells[i] = v.Text
			}
			return tea.SetClipboard(strings.Join(cells, "\t"))
		}
	}
	g.clamp(res)
	return nil
}

func (rp *ResultsPane) View(t *tab, spin string, focused bool) string {
	if t == nil || t.run == nil {
		return rp.center(mutedStyle.Italic(true).Render("Run a query to see results") + "\n\n" +
			hints("^⏎", "run", "F5", "run"))
	}
	r := t.run
	switch {
	case r.running:
		elapsed := time.Since(r.started).Truncate(100 * time.Millisecond)
		return rp.center(accentStyle.Render(spin) + " " + textStyle.Render("Running") +
			mutedStyle.Render("  "+elapsed.String()) + "\n\n" + hints("esc", "cancel"))
	case r.err != nil:
		return rp.errorView(r)
	case r.res.HasRows():
		return r.grid.render(r.res, rp.width, rp.height, focused)
	case r.res.HasAffected:
		return rp.center(okStyle.Render("✓ ") + textStyle.Bold(true).Render(plural(r.res.RowsAffected, "row")+" affected") +
			"\n\n" + mutedStyle.Render(formatDuration(r.res.Duration)))
	}
	return rp.center(okStyle.Render("✓ ") + textStyle.Bold(true).Render("Statement executed") +
		"\n\n" + mutedStyle.Render(formatDuration(r.res.Duration)))
}

func (rp *ResultsPane) center(s string) string {
	return lipgloss.Place(rp.width, rp.height, lipgloss.Center, lipgloss.Center, s)
}

// Status summarises the result for the pane's bottom border.
func (rp *ResultsPane) Status(t *tab, spin string) string {
	if t == nil || t.run == nil {
		return ""
	}
	r := t.run
	switch {
	case r.running:
		return accentStyle.Render(spin) + mutedStyle.Render(" running")
	case r.err != nil:
		return errorStyle.Render("✗ failed")
	case r.res.HasRows():
		s := okStyle.Render("✓ ") + textStyle.Render(plural(int64(len(r.res.Rows)), "row"))
		if r.res.Truncated {
			s += mutedStyle.Render(" (first " + strconv.Itoa(db.RowLimit) + ")")
		}
		return s + mutedStyle.Render(" · "+formatDuration(r.res.Duration))
	}
	return okStyle.Render("✓ ") + mutedStyle.Render(formatDuration(r.res.Duration))
}

// Footer shows the cell position and the current column's type.
func (rp *ResultsPane) Footer(t *tab, focused bool) string {
	if t == nil || t.run == nil || t.run.running || t.run.res == nil || !t.run.res.HasRows() {
		return ""
	}
	g, res := &t.run.grid, t.run.res
	col := res.Columns[g.col]
	pos := fmt.Sprintf("%d/%d · %s", min(g.row+1, len(res.Rows)), len(res.Rows), col.Name)
	if col.Type != "" {
		pos += " " + strings.ToLower(col.Type)
	}
	s := mutedStyle.Render(pos)
	if focused {
		s += "  " + hints("y", "copy")
	}
	return s
}

func plural(n int64, word string) string {
	s := groupDigits(n) + " " + word
	if n != 1 {
		s += "s"
	}
	return s
}

func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1 ms"
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + " ms"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + " s"
}

// errorView shows the message, the database's code and, when the driver
// says where, the offending line of the query with a caret.
func (rp *ResultsPane) errorView(r *run) string {
	e := r.err
	inner := max(rp.width-4, 10)
	var rows []string
	add := func(s ...string) { rows = append(rows, s...) }

	head := errorStyle.Bold(true).Render("✗ Query failed")
	if e.Code != "" {
		head += mutedStyle.Render("   " + e.Code)
	}
	add("", head, "")
	for _, l := range wrapLines(e.Message, inner, 6) {
		add(textStyle.Render(l))
	}

	if line, col, ok := errorLocation(r.query, *e); ok {
		lines := strings.Split(r.query, "\n")
		numW := len(strconv.Itoa(r.line + line + 1))
		gutter := func(n string) string {
			return mutedStyle.Render(fmt.Sprintf("%*s │ ", numW, n))
		}
		add("")
		if line > 0 {
			add(gutter(strconv.Itoa(r.line+line)) + mutedStyle.Render(ansi.Truncate(expandTabs(lines[line-1]), inner-numW-3, "…")))
		}
		add(gutter(strconv.Itoa(r.line+line+1)) + textStyle.Render(ansi.Truncate(expandTabs(lines[line]), inner-numW-3, "…")))
		if col >= 0 {
			caretCol := runewidth.StringWidth(expandTabs(string([]rune(lines[line])[:min(col, len([]rune(lines[line])))])))
			add(gutter("") + strings.Repeat(" ", caretCol) + errorStyle.Bold(true).Render("^"))
		}
	}

	label := func(name, text string) {
		for i, l := range wrapLines(text, inner-8, 4) {
			prefix := strings.Repeat(" ", 8)
			if i == 0 {
				prefix = mutedStyle.Render(fmt.Sprintf("%-8s", name))
			}
			add(prefix + titleStyle.Render(l))
		}
	}
	if e.Detail != "" {
		add("")
		label("Detail", e.Detail)
	}
	if e.Hint != "" {
		add("")
		label("Hint", e.Hint)
	}

	return lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))
}

// errorLocation maps an error position to a 0-based line and rune column
// inside the query; col is -1 when only the line is known.
func errorLocation(query string, e db.Error) (line, col int, ok bool) {
	lines := strings.Split(query, "\n")
	switch {
	case e.Position > 0:
		offset := e.Position - 1
		for i, l := range lines {
			n := len([]rune(l))
			if offset <= n {
				return i, offset, true
			}
			offset -= n + 1
		}
	case e.Line > 0 && e.Line <= len(lines):
		return e.Line - 1, -1, true
	}
	return 0, 0, false
}

func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

// ── Grid ────────────────────────────────────────────────────────────────────

// grid is the cursor and scroll position over a result set. It scrolls
// vertically by row and horizontally by whole columns.
type grid struct {
	row, col  int
	top, left int
	widths    []int
}

func (g *grid) clamp(res *db.Result) {
	g.row = min(max(g.row, 0), max(len(res.Rows)-1, 0))
	g.col = min(max(g.col, 0), len(res.Columns)-1)
}

func cellText(s string) string {
	return strings.NewReplacer("\r\n", "↵", "\n", "↵", "\r", "", "\t", " ").Replace(s)
}

func (g *grid) measure(res *db.Result) {
	if g.widths != nil {
		return
	}
	g.widths = make([]int, len(res.Columns))
	for i, c := range res.Columns {
		w := runewidth.StringWidth(c.Name)
		for _, row := range res.Rows {
			w = max(w, runewidth.StringWidth(cellText(row[i].Text)))
			if w >= maxColumnWidth {
				break
			}
		}
		g.widths[i] = min(max(w, 1), maxColumnWidth)
	}
}

const cellGap = 2

func (g *grid) render(res *db.Result, width, height int, focused bool) string {
	g.measure(res)
	g.clamp(res)

	gutterW := len(strconv.Itoa(max(len(res.Rows), 1))) + 1
	avail := width - gutterW - 1
	bodyH := max(height-2, 1)

	// Vertical scroll.
	if g.row < g.top {
		g.top = g.row
	}
	if g.row >= g.top+bodyH {
		g.top = g.row - bodyH + 1
	}
	// Horizontal scroll: move the first column until the cursor column fits.
	if g.col < g.left {
		g.left = g.col
	}
	for g.left < g.col && g.span(g.left, g.col) > avail {
		g.left++
	}

	// Columns that fit, the last one possibly clipped.
	var cols []int
	used := 0
	for c := g.left; c < len(res.Columns) && used < avail; c++ {
		cols = append(cols, c)
		used += g.widths[c] + cellGap
	}

	plain := lipgloss.NewStyle()
	var out []string

	// Header and rule.
	var h strings.Builder
	h.WriteString(strings.Repeat(" ", gutterW+1))
	for _, c := range cols {
		st := textStyle.Bold(true)
		if c == g.col {
			st = accentStyle.Bold(true)
		}
		h.WriteString(st.Render(pad(res.Columns[c].Name, g.widths[c], res.Columns[c].Numeric)))
		h.WriteString(strings.Repeat(" ", cellGap))
	}
	out = append(out, fit(h.String(), width, plain))
	rule := strings.Repeat("─", width)
	if g.left > 0 {
		rule = "‹" + rule[len("─"):]
	}
	if len(cols) > 0 && (cols[len(cols)-1] < len(res.Columns)-1 || used-cellGap > avail) {
		rule = strings.TrimSuffix(rule, "─") + "›"
	}
	out = append(out, borderStyle.Render(rule))

	if len(res.Rows) == 0 {
		out = append(out, strings.Repeat(" ", gutterW+1)+mutedStyle.Italic(true).Render("no rows"))
	}
	for r := g.top; r < min(g.top+bodyH, len(res.Rows)); r++ {
		current := r == g.row
		bg := func(s lipgloss.Style) lipgloss.Style {
			if current && focused {
				return s.Background(colorCursor)
			}
			return s
		}
		var b strings.Builder
		num := mutedStyle
		if current {
			num = accentStyle
		}
		b.WriteString(bg(num).Render(fmt.Sprintf("%*d", gutterW, r+1)) + bg(plain).Render(" "))
		for _, c := range cols {
			v := res.Rows[r][c]
			st := textStyle
			if v.Null {
				st = mutedStyle.Italic(true)
			}
			st = bg(st)
			if current && c == g.col && focused {
				st = st.Background(colorBorder).Bold(true)
			}
			b.WriteString(st.Render(pad(cellText(v.Text), g.widths[c], res.Columns[c].Numeric)))
			b.WriteString(bg(plain).Render(strings.Repeat(" ", cellGap)))
		}
		out = append(out, fit(b.String(), width, bg(plain)))
	}
	return strings.Join(out, "\n")
}

// span is the width of columns from..to inclusive.
func (g *grid) span(from, to int) int {
	w := 0
	for c := from; c <= to; c++ {
		w += g.widths[c] + cellGap
	}
	return w - cellGap
}

// pad fits s into width cells, truncating with an ellipsis, and aligns it.
func pad(s string, width int, right bool) string {
	if runewidth.StringWidth(s) > width {
		s = runewidth.Truncate(s, width, "…")
	}
	gap := strings.Repeat(" ", max(width-runewidth.StringWidth(s), 0))
	if right {
		return gap + s
	}
	return s + gap
}

// fit clips a rendered row to width and pads it with the given style.
func fit(s string, width int, padStyle lipgloss.Style) string {
	s = ansi.Truncate(s, width, "")
	if w := lipgloss.Width(s); w < width {
		s += padStyle.Render(strings.Repeat(" ", width-w))
	}
	return s
}
