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
	running bool // executing; the result is not back yet

	res *db.Result
	err *db.Error

	grid grid
}

// fetching reports whether rows are still arriving.
func (r *run) fetching() bool {
	return r.res != nil && r.res.Rows != nil && !r.res.Rows.Progress().Done
}

// busy reports whether the run still uses the tab's session.
func (r *run) busy() bool { return r != nil && (r.running || r.fetching()) }

type runDoneMsg struct {
	tab     *tab
	gen     int
	res     *db.Result
	err     error
	session *sql.Conn
}

// rowsMsg reports that more rows arrived, or that fetching ended.
type rowsMsg struct {
	tab *tab
	gen int
}

// rowsRefresh is how often the grid redraws while rows arrive.
const rowsRefresh = 80 * time.Millisecond

// watchRows waits for the next batch of rows.
func watchRows(t *tab, gen int, set *db.RowSet) tea.Cmd {
	return func() tea.Msg {
		<-set.Updated()
		if !set.Progress().Done {
			time.Sleep(rowsRefresh) // batch redraws while rows pour in
		}
		return rowsMsg{t, gen}
	}
}

// Run executes the selection, or the whole tab, on the tab's session.
func (q *QueryPane) Run(t *tab) tea.Cmd {
	if t.run.busy() {
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
	opts := db.RunOptions{MemoryLimit: q.memoryLimit, Cancel: cancel}
	exec := func() tea.Msg {
		for attempt := 0; ; attempt++ {
			if session == nil {
				s, err := pool.Session(ctx, database)
				if err != nil {
					cancel()
					return runDoneMsg{tab: t, gen: gen, err: err}
				}
				session = s
			}
			// Run releases the context itself, when the statement or the
			// fetch of its rows ends.
			res, err := db.Run(ctx, session, text, opts)
			if err != nil && attempt == 0 && db.IsConnLost(err) {
				go pool.Release(session) // the connection went away; retry on a fresh one
				session = nil
				ctx, cancel = context.WithCancel(context.Background())
				opts.Cancel = cancel
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

// running counts tabs whose runs still use their session.
func (q *QueryPane) running() int {
	n := 0
	for _, t := range q.tabs {
		if t.run.busy() {
			n++
		}
	}
	return n
}

// finish records the outcome of a run and starts watching its rows.
func (q *QueryPane) finish(msg runDoneMsg) tea.Cmd {
	t := msg.tab
	if msg.session != nil && msg.session != t.session {
		t.dropSession()
		t.session = msg.session
	}
	r := t.run
	if r == nil || r.gen != msg.gen {
		t.releaseIfStale()
		return nil
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
	t.releaseIfStale()
	if r.res != nil && r.res.Rows != nil {
		return watchRows(t, r.gen, r.res.Rows)
	}
	return nil
}

// rowsArrived handles a rowsMsg: keep watching until fetching ends.
func (q *QueryPane) rowsArrived(msg rowsMsg) tea.Cmd {
	t := msg.tab
	r := t.run
	if r == nil || r.gen != msg.gen || r.res == nil || r.res.Rows == nil {
		return nil
	}
	set := r.res.Rows
	p := set.Progress()
	if !p.Done {
		return watchRows(t, r.gen, set)
	}
	if p.Err != nil && p.Rows == 0 {
		e := db.Describe(p.Err) // failed before any row: show it as an error
		r.err = &e
	}
	t.releaseIfStale()
	return nil
}

// releaseIfStale drops the session once the run is over if the tab was
// closed or moved to another context meanwhile.
func (t *tab) releaseIfStale() {
	if (t.closed || t.sessionCtx != t.ctx) && !t.run.busy() {
		t.dropSession()
	}
}

// cancelRun stops the tab's statement, or the fetch of its rows; rows
// fetched so far stay.
func (t *tab) cancelRun() bool {
	switch {
	case t.run == nil:
		return false
	case t.run.running:
		t.run.cancel()
		return true
	case t.run.fetching():
		t.run.res.Rows.Stop()
		return true
	}
	return false
}

// dropSession gives the session back. Closing waits for open rows, so it
// happens in the background; callers stop any fetch first.
func (t *tab) dropSession() {
	if t.session == nil {
		return
	}
	s := t.session
	if conn := t.sessionCtx.conn; conn != nil {
		go conn.pool.Release(s)
	} else {
		go s.Close()
	}
	t.session = nil
}

// ── Results pane ────────────────────────────────────────────────────────────

// ResultsPane shows the outcome of the active tab's last run.
type ResultsPane struct {
	width, height int
	runKey        string // the run key for hints, as the terminal reports it
	memoryLimit   int64  // for the "stopped at the limit" status
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
	g, set := &r.grid, r.res.Rows
	n, cols := set.Len(), len(set.Columns())
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
		g.row = n - 1
	case "home", "0":
		g.col = 0
	case "end", "$":
		g.col = cols - 1
	case "y":
		if g.row < n {
			return tea.SetClipboard(set.Cell(g.row, g.col).Text)
		}
	case "Y":
		if g.row < n {
			cells := make([]string, cols)
			for c := range cells {
				cells[c] = set.Cell(g.row, c).Text
			}
			return tea.SetClipboard(strings.Join(cells, "\t"))
		}
	}
	g.clamp(set)
	return nil
}

func (rp *ResultsPane) View(t *tab, spin string, focused bool) string {
	if t == nil || t.run == nil {
		return rp.center(mutedStyle.Italic(true).Render("Run a query to see results") + "\n\n" +
			hints(rp.runKey, "run"))
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
		return r.grid.render(r.res.Rows, rp.width, rp.height, focused)
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

// Status summarises the result for the pane's bottom border: the row
// count, live while rows arrive, and why fetching stopped early.
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
	case !r.res.HasRows():
		return okStyle.Render("✓ ") + mutedStyle.Render(formatDuration(r.res.Duration))
	}

	p := r.res.Rows.Progress()
	rows := textStyle.Render(plural(int64(p.Rows), "row"))
	if !p.Done {
		return accentStyle.Render(spin+" ") + rows + mutedStyle.Render(" · fetching")
	}
	s := okStyle.Render("✓ ") + rows
	switch {
	case p.Err != nil:
		s = errorStyle.Render("✗ ") + rows + errorStyle.Render(" · "+firstLine(db.Describe(p.Err).Message))
	case p.Stopped == db.StopLimit:
		s += mutedStyle.Render(" · stopped at the " + formatBytes(rp.memoryLimit) + " limit")
	case p.Stopped == db.StopCancelled:
		s += mutedStyle.Render(" · stopped")
	}
	return s + mutedStyle.Render(" · "+formatDuration(r.res.Duration+p.Elapsed))
}

// Footer shows the cell position and the current column's type.
func (rp *ResultsPane) Footer(t *tab, focused bool) string {
	if t == nil || t.run == nil || t.run.running || t.run.res == nil || !t.run.res.HasRows() {
		return ""
	}
	g, set := &t.run.grid, t.run.res.Rows
	n := set.Len()
	col := set.Columns()[g.col]
	pos := fmt.Sprintf("%s/%s · %s", groupDigits(int64(min(g.row+1, n))), groupDigits(int64(n)), col.Name)
	if col.Type != "" {
		pos += " " + strings.ToLower(col.Type)
	}
	s := mutedStyle.Render(pos)
	switch {
	case focused && t.run.fetching():
		s += "  " + hints("esc", "stop")
	case focused:
		s += "  " + hints("y", "copy")
	}
	return s
}

// formatBytes renders a size in binary units: 512 MB, 1.5 GB.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', -1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatInt(n>>20, 10) + " MB"
	}
	return strconv.FormatInt(n>>10, 10) + " KB"
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
	measured  int // rows the widths are taken from
}

func (g *grid) clamp(set *db.RowSet) {
	g.row = min(max(g.row, 0), max(set.Len()-1, 0))
	g.col = min(max(g.col, 0), len(set.Columns())-1)
}

func cellText(s string) string {
	return strings.NewReplacer("\r\n", "↵", "\n", "↵", "\r", "", "\t", " ").Replace(s)
}

// measureRows is how many leading rows set the initial column widths.
// Rows further down widen columns when they scroll into view, so values
// are never cut short just for being deep in a large result.
const measureRows = 2000

// measure sizes columns from their names and the first rows, as they arrive.
func (g *grid) measure(set *db.RowSet) {
	cols := set.Columns()
	if g.widths == nil {
		g.widths = make([]int, len(cols))
		for i, c := range cols {
			g.widths[i] = min(max(runewidth.StringWidth(c.Name), 1), maxColumnWidth)
		}
	}
	upTo := min(set.Len(), measureRows)
	for r := g.measured; r < upTo; r++ {
		for c := range cols {
			if g.widths[c] < maxColumnWidth {
				g.widths[c] = min(max(g.widths[c], runewidth.StringWidth(cellText(set.Cell(r, c).Text))), maxColumnWidth)
			}
		}
	}
	g.measured = max(g.measured, upTo)
}

// widen grows columns to fit the rows from..to, the ones on screen.
func (g *grid) widen(set *db.RowSet, from, to int) {
	for r := max(from, g.measured); r < to; r++ {
		for c := range g.widths {
			if g.widths[c] < maxColumnWidth {
				g.widths[c] = min(max(g.widths[c], runewidth.StringWidth(cellText(set.Cell(r, c).Text))), maxColumnWidth)
			}
		}
	}
}

const cellGap = 2

func (g *grid) render(set *db.RowSet, width, height int, focused bool) string {
	g.measure(set)
	g.clamp(set)
	columns, n := set.Columns(), set.Len()

	gutterW := len(strconv.Itoa(max(n, 1))) + 1
	avail := width - gutterW - 1
	bodyH := max(height-2, 1)

	// Vertical scroll.
	if g.row < g.top {
		g.top = g.row
	}
	if g.row >= g.top+bodyH {
		g.top = g.row - bodyH + 1
	}
	g.widen(set, g.top, min(g.top+bodyH, n))
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
	for c := g.left; c < len(columns) && used < avail; c++ {
		cols = append(cols, c)
		used += g.widths[c] + cellGap
	}
	numeric := make([]bool, len(columns))
	for _, c := range cols {
		numeric[c] = set.Numeric(c)
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
		h.WriteString(st.Render(pad(columns[c].Name, g.widths[c], numeric[c])))
		h.WriteString(strings.Repeat(" ", cellGap))
	}
	out = append(out, fit(h.String(), width, plain))
	rule := strings.Repeat("─", width)
	if g.left > 0 {
		rule = "‹" + rule[len("─"):]
	}
	if len(cols) > 0 && (cols[len(cols)-1] < len(columns)-1 || used-cellGap > avail) {
		rule = strings.TrimSuffix(rule, "─") + "›"
	}
	out = append(out, borderStyle.Render(rule))

	if n == 0 {
		note := "no rows"
		if !set.Progress().Done {
			note = "waiting for rows…"
		}
		out = append(out, strings.Repeat(" ", gutterW+1)+mutedStyle.Italic(true).Render(note))
	}
	for r := g.top; r < min(g.top+bodyH, n); r++ {
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
			v := set.Cell(r, c)
			st := textStyle
			if v.Null {
				st = mutedStyle.Italic(true)
			}
			st = bg(st)
			if current && c == g.col && focused {
				st = st.Background(colorBorder).Bold(true)
			}
			b.WriteString(st.Render(pad(cellText(v.Text), g.widths[c], numeric[c])))
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
