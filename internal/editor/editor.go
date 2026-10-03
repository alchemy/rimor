// Package editor is a multi-line text editor for SQL with syntax
// highlighting, selection, clipboard and undo.
package editor

import (
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/mattn/go-runewidth"

	tea "charm.land/bubbletea/v2"
)

// TabWidth is the width of a tab stop; the tab key inserts spaces.
const TabWidth = 4

const undoLimit = 500

type pos struct{ row, col int }

func (a pos) less(b pos) bool { return a.row < b.row || (a.row == b.row && a.col < b.col) }

type snapshot struct {
	lines  [][]rune
	cursor pos
}

// editKind groups consecutive edits into one undo step.
type editKind int

const (
	editNone editKind = iota
	editInsert
	editDelete
	editOther
)

type Model struct {
	// Placeholder is shown, muted, while the buffer is empty.
	Placeholder string

	lines  [][]rune
	cursor pos
	anchor *pos // selection start; nil when nothing is selected
	goal   int  // display column kept across vertical moves; -1 for none

	top, left     int
	width, height int
	focused       bool
	version       int

	undo, redo []snapshot
	last       editKind
	clipboard  string

	lexer          chroma.Lexer
	classes        [][]class
	classesVersion int

	theme  Theme
	styles [classCount][2]lipgloss.Style // [class][selected]
	gutter [2]lipgloss.Style             // [current line]
	muted  lipgloss.Style
}

func New(theme Theme) *Model {
	m := &Model{
		lines:          [][]rune{{}},
		goal:           -1,
		lexer:          lexerFor(SQL),
		classesVersion: -1,
	}
	m.SetTheme(theme)
	return m
}

// SetTheme changes the colours.
func (m *Model) SetTheme(theme Theme) {
	m.theme = theme
	for c := range classCount {
		base := lipgloss.NewStyle()
		if col := theme.color(c); col != nil {
			base = base.Foreground(col)
		}
		if c == clsComment {
			base = base.Italic(true)
		}
		if theme.FaintMuted && (c == clsComment || c == clsPunctuation) {
			base = base.Faint(true)
		}
		m.styles[c][0] = base
		m.styles[c][1] = base.Background(theme.Selection)
		if theme.ReverseSelection {
			m.styles[c][1] = base.Reverse(true)
		}
	}
	m.gutter[0] = lipgloss.NewStyle().Foreground(theme.LineNumber).Faint(theme.FaintMuted)
	m.gutter[1] = lipgloss.NewStyle().Foreground(theme.CurrentLineNumber).Bold(true)
	m.muted = lipgloss.NewStyle().Foreground(theme.Placeholder).Italic(true).Faint(theme.FaintMuted)
}

// SetLanguage picks the SQL dialect used for highlighting.
func (m *Model) SetLanguage(lang Language) {
	m.lexer = lexerFor(lang)
	m.classesVersion = -1
}

// SetText replaces the buffer and forgets the undo history.
func (m *Model) SetText(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	m.lines = nil
	for _, l := range strings.Split(s, "\n") {
		m.lines = append(m.lines, []rune(l))
	}
	m.cursor, m.anchor, m.goal = pos{}, nil, -1
	m.top, m.left = 0, 0
	m.undo, m.redo, m.last = nil, nil, editNone
	m.version++
}

func (m *Model) Text() string {
	var b strings.Builder
	for i, l := range m.lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(string(l))
	}
	return b.String()
}

// Version changes on every edit; compare versions to detect changes.
func (m *Model) Version() int { return m.version }

// CursorPosition is the 0-based line and column of the cursor.
func (m *Model) CursorPosition() (line, col int) { return m.cursor.row, m.cursor.col }

// SetCursorPosition moves the cursor, clamped to the buffer.
func (m *Model) SetCursorPosition(line, col int) {
	m.cursor.row = min(max(line, 0), len(m.lines)-1)
	m.cursor.col = min(max(col, 0), len(m.lines[m.cursor.row]))
	m.anchor = nil
	m.scroll()
}

// SelectedText returns the selection, or "" when nothing is selected.
func (m *Model) SelectedText() string {
	a, b, ok := m.selection()
	if !ok {
		return ""
	}
	return m.textRange(a, b)
}

// SelectionStart is the 0-based line and column where the selection begins.
func (m *Model) SelectionStart() (line, col int, ok bool) {
	a, _, ok := m.selection()
	return a.row, a.col, ok
}

func (m *Model) Focus()        { m.focused = true }
func (m *Model) Blur()         { m.focused = false }
func (m *Model) Focused() bool { return m.focused }

func (m *Model) SetSize(width, height int) {
	m.width, m.height = width, height
	m.scroll()
}

// Cursor is the cursor's cell inside the editor's view, for placing the
// terminal cursor. ok is false when the editor is blurred.
func (m *Model) Cursor() (x, y int, ok bool) {
	if !m.focused {
		return 0, 0, false
	}
	x = m.gutterWidth() + m.displayCol(m.cursor) - m.left
	y = m.cursor.row - m.top
	return x, y, y >= 0 && y < m.height && x < m.width
}

// ── Updates ─────────────────────────────────────────────────────────────────

func (m *Model) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.PasteMsg:
		m.checkpoint(editOther)
		m.insert(msg.Content)
	case tea.KeyPressMsg:
		cmd = m.handleKey(msg)
	default:
		return nil
	}
	m.scroll()
	return cmd
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if m.handleMove(key) {
		return nil
	}

	switch key {
	case "enter":
		m.checkpoint(editOther)
		m.insert("\n" + m.indentOf(m.cursor.row, m.cursor.col))
	case "tab":
		m.checkpoint(editOther)
		if a, b, ok := m.selection(); ok && a.row != b.row {
			m.indentLines(a.row, b.row)
		} else {
			m.insert(strings.Repeat(" ", TabWidth-m.displayCol(m.cursor)%TabWidth))
		}
	case "shift+tab":
		m.checkpoint(editOther)
		a, b, ok := m.selection()
		if !ok {
			a, b = m.cursor, m.cursor
		}
		m.dedentLines(a.row, b.row)
	case "backspace":
		m.checkpoint(editDelete)
		if !m.deleteSelection() {
			m.deleteRange(m.prevPos(m.cursor), m.cursor)
		}
	case "delete":
		m.checkpoint(editDelete)
		if !m.deleteSelection() {
			m.deleteRange(m.cursor, m.nextPos(m.cursor))
		}
	case "ctrl+backspace", "alt+backspace", "ctrl+h":
		m.checkpoint(editOther)
		if !m.deleteSelection() {
			m.deleteRange(m.wordLeft(m.cursor), m.cursor)
		}
	case "ctrl+delete", "alt+delete", "alt+d":
		m.checkpoint(editOther)
		if !m.deleteSelection() {
			m.deleteRange(m.cursor, m.wordRight(m.cursor))
		}

	case "ctrl+a":
		m.anchor = &pos{}
		last := len(m.lines) - 1
		m.cursor = pos{last, len(m.lines[last])}
	case "esc":
		m.anchor = nil
	case "ctrl+z":
		m.undoStep(&m.undo, &m.redo)
	case "ctrl+y", "ctrl+shift+z":
		m.undoStep(&m.redo, &m.undo)

	case "ctrl+c", "ctrl+x":
		text, ok := m.SelectedText(), true
		if text == "" {
			// No selection: the whole line, as most editors do.
			text, ok = string(m.lines[m.cursor.row])+"\n", false
		}
		m.clipboard = text
		if key == "ctrl+x" {
			m.checkpoint(editOther)
			if ok {
				m.deleteSelection()
			} else {
				m.deleteLine(m.cursor.row)
			}
		}
		return tea.SetClipboard(text)
	case "ctrl+v":
		if m.clipboard != "" {
			m.checkpoint(editOther)
			m.insert(m.clipboard)
		}

	default:
		if msg.Text == "" || msg.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModSuper) != 0 {
			return nil
		}
		m.checkpoint(editInsert)
		m.insert(msg.Text)
	}
	m.goal = -1
	return nil
}

// handleMove applies cursor movement keys, extending the selection when
// shift is held.
func (m *Model) handleMove(key string) bool {
	base := strings.Replace(key, "shift+", "", 1)
	selecting := base != key

	vertical := false
	var to pos
	switch base {
	case "left":
		if a, _, ok := m.selection(); ok && !selecting {
			to = a
		} else {
			to = m.prevPos(m.cursor)
		}
	case "right":
		if _, b, ok := m.selection(); ok && !selecting {
			to = b
		} else {
			to = m.nextPos(m.cursor)
		}
	case "ctrl+left", "alt+left", "alt+b":
		to = m.wordLeft(m.cursor)
	case "ctrl+right", "alt+right", "alt+f":
		to = m.wordRight(m.cursor)
	case "up":
		to, vertical = m.vertical(-1), true
	case "down":
		to, vertical = m.vertical(1), true
	case "pgup":
		to, vertical = m.vertical(-max(m.height-1, 1)), true
	case "pgdown":
		to, vertical = m.vertical(max(m.height-1, 1)), true
	case "home":
		first := m.firstNonSpace(m.cursor.row)
		to = pos{m.cursor.row, first}
		if m.cursor.col == first {
			to.col = 0
		}
	case "end":
		to = pos{m.cursor.row, len(m.lines[m.cursor.row])}
	case "ctrl+home":
		to = pos{}
	case "ctrl+end":
		last := len(m.lines) - 1
		to = pos{last, len(m.lines[last])}
	default:
		return false
	}

	if selecting && m.anchor == nil {
		anchor := m.cursor
		m.anchor = &anchor
	} else if !selecting {
		m.anchor = nil
	}
	if !vertical {
		m.goal = -1
	}
	m.cursor = to
	m.last = editNone
	return true
}

func (m *Model) vertical(delta int) pos {
	if m.goal < 0 {
		m.goal = m.displayCol(m.cursor)
	}
	row := min(max(m.cursor.row+delta, 0), len(m.lines)-1)
	if row == m.cursor.row && delta != 0 {
		// Moving past the first or last line goes to its start or end.
		if delta < 0 {
			return pos{row, 0}
		}
		return pos{row, len(m.lines[row])}
	}
	return pos{row, m.colForDisplay(row, m.goal)}
}

// ── Editing primitives ──────────────────────────────────────────────────────

// checkpoint records an undo step, merging runs of typing or deleting.
func (m *Model) checkpoint(kind editKind) {
	if kind == m.last && (kind == editInsert || kind == editDelete) {
		return
	}
	m.undo = append(m.undo, m.snapshot())
	if len(m.undo) > undoLimit {
		m.undo = m.undo[1:]
	}
	m.redo = nil
	m.last = kind
}

func (m *Model) snapshot() snapshot {
	lines := make([][]rune, len(m.lines))
	for i, l := range m.lines {
		lines[i] = append([]rune(nil), l...)
	}
	return snapshot{lines: lines, cursor: m.cursor}
}

func (m *Model) undoStep(from, to *[]snapshot) {
	if len(*from) == 0 {
		return
	}
	s := (*from)[len(*from)-1]
	*from = (*from)[:len(*from)-1]
	*to = append(*to, m.snapshot())
	m.lines, m.cursor, m.anchor = s.lines, s.cursor, nil
	m.last = editNone
	m.version++
}

func (m *Model) selection() (a, b pos, ok bool) {
	if m.anchor == nil || *m.anchor == m.cursor {
		return pos{}, pos{}, false
	}
	a, b = *m.anchor, m.cursor
	if b.less(a) {
		a, b = b, a
	}
	return a, b, true
}

func (m *Model) deleteSelection() bool {
	a, b, ok := m.selection()
	if ok {
		m.deleteRange(a, b)
	}
	m.anchor = nil
	return ok
}

func (m *Model) textRange(a, b pos) string {
	if a.row == b.row {
		return string(m.lines[a.row][a.col:b.col])
	}
	var s strings.Builder
	s.WriteString(string(m.lines[a.row][a.col:]))
	for r := a.row + 1; r < b.row; r++ {
		s.WriteByte('\n')
		s.WriteString(string(m.lines[r]))
	}
	s.WriteByte('\n')
	s.WriteString(string(m.lines[b.row][:b.col]))
	return s.String()
}

// deleteRange removes the text between a and b (a before b).
func (m *Model) deleteRange(a, b pos) {
	if a == b {
		return
	}
	tail := m.lines[b.row][b.col:]
	m.lines[a.row] = append(m.lines[a.row][:a.col:a.col], tail...)
	m.lines = append(m.lines[:a.row+1], m.lines[b.row+1:]...)
	m.cursor = a
	m.version++
}

func (m *Model) deleteLine(row int) {
	if len(m.lines) == 1 {
		m.lines[0] = nil
	} else {
		m.lines = append(m.lines[:row], m.lines[row+1:]...)
	}
	m.cursor = pos{min(row, len(m.lines)-1), 0}
	m.version++
}

// insert replaces the selection, if any, with s.
func (m *Model) insert(s string) {
	m.deleteSelection()
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.Split(s, "\n")

	line := m.lines[m.cursor.row]
	head := append([]rune(nil), line[:m.cursor.col]...)
	tail := append([]rune(nil), line[m.cursor.col:]...)

	newLines := make([][]rune, len(parts))
	for i, p := range parts {
		newLines[i] = []rune(p)
	}
	newLines[0] = append(head, newLines[0]...)
	last := len(newLines) - 1
	col := len(newLines[last])
	newLines[last] = append(newLines[last], tail...)

	rest := append([][]rune(nil), m.lines[m.cursor.row+1:]...)
	m.lines = append(append(m.lines[:m.cursor.row], newLines...), rest...)
	m.cursor = pos{m.cursor.row + last, col}
	m.version++
}

func (m *Model) indentLines(from, to int) {
	pad := []rune(strings.Repeat(" ", TabWidth))
	for r := from; r <= to; r++ {
		if len(m.lines[r]) > 0 {
			m.lines[r] = append(append([]rune(nil), pad...), m.lines[r]...)
		}
	}
	m.shiftSelection(from, to, TabWidth)
	m.version++
}

func (m *Model) dedentLines(from, to int) {
	removed := map[int]int{}
	for r := from; r <= to; r++ {
		n := 0
		for n < TabWidth && n < len(m.lines[r]) && m.lines[r][n] == ' ' {
			n++
		}
		if n == 0 && len(m.lines[r]) > 0 && m.lines[r][0] == '\t' {
			n = 1
		}
		m.lines[r] = m.lines[r][n:]
		removed[r] = n
	}
	adjust := func(p *pos) { p.col = max(p.col-removed[p.row], 0) }
	adjust(&m.cursor)
	if m.anchor != nil {
		adjust(m.anchor)
	}
	m.version++
}

// shiftSelection moves the cursor and anchor after lines were indented.
func (m *Model) shiftSelection(from, to, by int) {
	for _, p := range []*pos{&m.cursor, m.anchor} {
		if p != nil && p.row >= from && p.row <= to && len(m.lines[p.row]) > 0 {
			p.col += by
		}
	}
}

func (m *Model) indentOf(row, upTo int) string {
	line := m.lines[row]
	n := 0
	for n < len(line) && n < upTo && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return string(line[:n])
}

func (m *Model) firstNonSpace(row int) int {
	line := m.lines[row]
	for i, r := range line {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return len(line)
}

func (m *Model) prevPos(p pos) pos {
	switch {
	case p.col > 0:
		return pos{p.row, p.col - 1}
	case p.row > 0:
		return pos{p.row - 1, len(m.lines[p.row-1])}
	}
	return p
}

func (m *Model) nextPos(p pos) pos {
	switch {
	case p.col < len(m.lines[p.row]):
		return pos{p.row, p.col + 1}
	case p.row < len(m.lines)-1:
		return pos{p.row + 1, 0}
	}
	return p
}

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func (m *Model) wordLeft(p pos) pos {
	if p.col == 0 {
		return m.prevPos(p)
	}
	line := m.lines[p.row]
	c := p.col
	for c > 0 && unicode.IsSpace(line[c-1]) {
		c--
	}
	if c > 0 && isWord(line[c-1]) {
		for c > 0 && isWord(line[c-1]) {
			c--
		}
	} else if c > 0 {
		c--
	}
	return pos{p.row, c}
}

func (m *Model) wordRight(p pos) pos {
	line := m.lines[p.row]
	if p.col == len(line) {
		return m.nextPos(p)
	}
	c := p.col
	if isWord(line[c]) {
		for c < len(line) && isWord(line[c]) {
			c++
		}
	} else if !unicode.IsSpace(line[c]) {
		c++
	}
	for c < len(line) && unicode.IsSpace(line[c]) {
		c++
	}
	return pos{p.row, c}
}

// ── Layout ──────────────────────────────────────────────────────────────────

func runeWidth(r rune, col int) int {
	if r == '\t' {
		return TabWidth - col%TabWidth
	}
	return max(runewidth.RuneWidth(r), 0)
}

func (m *Model) displayCol(p pos) int {
	col := 0
	for _, r := range m.lines[p.row][:p.col] {
		col += runeWidth(r, col)
	}
	return col
}

func (m *Model) colForDisplay(row, target int) int {
	col := 0
	for i, r := range m.lines[row] {
		w := runeWidth(r, col)
		if col+w > target {
			return i
		}
		col += w
	}
	return len(m.lines[row])
}

func (m *Model) gutterWidth() int {
	digits := len(itoa(len(m.lines)))
	return max(digits, 3) + 2
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func (m *Model) textWidth() int { return max(m.width-m.gutterWidth(), 1) }

// scroll keeps the cursor inside the viewport.
func (m *Model) scroll() {
	if m.height <= 0 {
		return
	}
	if m.cursor.row < m.top {
		m.top = m.cursor.row
	}
	if m.cursor.row >= m.top+m.height {
		m.top = m.cursor.row - m.height + 1
	}
	m.top = min(m.top, max(len(m.lines)-1, 0))

	col, tw := m.displayCol(m.cursor), m.textWidth()
	if col < m.left {
		m.left = max(col-tw/4, 0)
	}
	if col >= m.left+tw {
		m.left = col - tw + 1
	}
}

// ── View ────────────────────────────────────────────────────────────────────

func (m *Model) View() string {
	if m.classesVersion != m.version {
		m.classes = highlight(m.lexer, m.lines)
		m.classesVersion = m.version
	}

	gw := m.gutterWidth()
	tw := m.textWidth()
	selA, selB, hasSel := m.selection()
	empty := len(m.lines) == 1 && len(m.lines[0]) == 0

	rows := make([]string, m.height)
	for y := range m.height {
		r := m.top + y
		if r >= len(m.lines) {
			rows[y] = strings.Repeat(" ", m.width)
			continue
		}

		num := itoa(r + 1)
		g := m.gutter[0]
		if r == m.cursor.row && m.focused {
			g = m.gutter[1]
		}
		gutter := g.Render(strings.Repeat(" ", gw-2-len(num))+num) + "  "

		var body string
		if empty && y == 0 && m.Placeholder != "" {
			body = m.muted.Render(truncate(m.Placeholder, tw))
		} else {
			body = m.renderLine(r, tw, selA, selB, hasSel)
		}
		if pad := m.width - gw - lipgloss.Width(body); pad > 0 {
			body += strings.Repeat(" ", pad)
		}
		rows[y] = gutter + body
	}
	return strings.Join(rows, "\n")
}

func truncate(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return string(r[:w])
	}
	return s
}

// renderLine draws the visible slice of a line, grouping runs of runes
// that share a style.
func (m *Model) renderLine(row, width int, selA, selB pos, hasSel bool) string {
	line := m.lines[row]
	classes := m.classes[row]
	selected := func(c int) bool {
		p := pos{row, c}
		return hasSel && !p.less(selA) && p.less(selB)
	}

	var b strings.Builder
	var run strings.Builder
	runCls, runSel := class(0), false
	flush := func() {
		if run.Len() > 0 {
			sel := 0
			if runSel {
				sel = 1
			}
			b.WriteString(m.styles[runCls][sel].Render(run.String()))
			run.Reset()
		}
	}

	col, used := 0, 0
	for i, r := range line {
		w := runeWidth(r, col)
		start := col
		col += w
		if col <= m.left {
			continue
		}
		cell := string(r)
		if r == '\t' || start < m.left {
			cell = strings.Repeat(" ", col-max(start, m.left)) // tab, or a wide rune cut by the edge
			w = col - max(start, m.left)
		}
		if used+w > width {
			break
		}
		cls, sel := classes[i], selected(i)
		if cls != runCls || sel != runSel {
			flush()
			runCls, runSel = cls, sel
		}
		run.WriteString(cell)
		used += w
	}
	flush()

	// Show a selected line break as one highlighted cell.
	if hasSel && row >= selA.row && row < selB.row && used < width && col >= m.left {
		b.WriteString(m.styles[clsText][1].Render(" "))
	}
	return b.String()
}
