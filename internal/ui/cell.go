package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
	"rimor.dev/internal/editor"
)

// The cell popup shows a result cell's full value, selected for copying,
// and edits it when rimor can tell which table row the cell comes from.

const cellTimeout = 15 * time.Second

type cellPhase int

const (
	cellViewing cellPhase = iota // the value only; reason says why
	cellLoading                  // finding the row and its current value
	cellEditing
	cellSaving
)

// openCellMsg asks for the popup of a result cell.
type openCellMsg struct {
	tab      *tab
	row, col int
}

// cellReadyMsg brings what editing needs, or why it is not possible.
type cellReadyMsg struct {
	popup  *cellPopup
	origin *db.Origin
	// originErr is why the whole result cannot be edited; cached on the run.
	originErr error
	cell      *db.Cell
	current   *string // the value as the database writes it; nil for NULL
	reason    string  // why this cell cannot be edited
	err       error
}

type cellSavedMsg struct {
	popup *cellPopup
	value db.Value
	err   error
}

type cellPopup struct {
	tab      *tab
	run      *run
	row, col int
	column   db.Column

	ed     *editor.Model
	width  int
	phase  cellPhase
	reason string // why the cell is only shown
	err    string // the last failure, in red

	cell     *db.Cell
	old      *string // the value editing started from, nil for NULL
	original string  // the editor text editing started from
	null     bool    // the new value is NULL
	nullAt   int     // editor version when NULL was chosen

	// json is a JSON column: highlighted, formatted with ctrl+f and
	// validated before saving. normalized types (jsonb, SQL Server's json)
	// store their own form, so they open pretty-printed; json and text
	// columns keep the text as written.
	json, normalized bool
	// multiline values take enter as a new line and save with ctrl+s.
	multiline bool
	maxWidth  int
	maxHeight int
}

// jsonColumn reports whether a column holds JSON, and whether the
// database stores it in its own normalised form.
func jsonColumn(d db.Driver, typ string) (isJSON, normalized bool) {
	switch strings.ToUpper(typ) {
	case "JSONB":
		return true, true
	case "JSON":
		return true, d == db.SQLServer // SQL Server 2025's json is binary
	}
	return false, false
}

// prettyJSON indents valid JSON; anything else comes back unchanged.
func prettyJSON(s string) string {
	var b bytes.Buffer
	if json.Indent(&b, []byte(s), "", "  ") != nil {
		return s
	}
	return b.String()
}

// jsonError describes invalid JSON with the line and column of the fault.
func jsonError(s string) (msg string, line, col int, ok bool) {
	var v any
	err := json.Unmarshal([]byte(s), &v)
	if err == nil {
		return "", 0, 0, true
	}
	offset := len(s)
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		offset = int(syn.Offset)
	}
	offset = min(max(offset, 0), len(s))
	before := s[:offset]
	line = strings.Count(before, "\n")
	col = utf8.RuneCountInString(before[strings.LastIndexByte(before, '\n')+1:])
	reason := strings.TrimPrefix(err.Error(), "json: ")
	if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(reason, "unexpected end") {
		reason = "the JSON ends too early"
	}
	return fmt.Sprintf("Invalid JSON at line %d, column %d: %s.", line+1, col+1, reason), line, col, false
}

func newCellPopup(t *tab, row, col, maxWidth int) *cellPopup {
	set := t.run.res.Rows
	p := &cellPopup{tab: t, run: t.run, row: row, col: col, column: set.Columns()[col]}
	p.json, p.normalized = jsonColumn(t.ctx.conn.cfg().Driver, p.column.Type)
	p.ed = editor.New(editorTheme)
	p.ed.NoLineNumbers = true
	p.ed.ReadOnly = true
	if p.json {
		p.ed.SetLanguage(editor.JSON)
	}
	v := set.Cell(row, col)
	if v.Null {
		p.ed.Placeholder = "NULL"
	} else {
		text := v.Text
		if p.normalized {
			text = prettyJSON(text)
		}
		p.ed.SetText(text)
	}
	p.multiline = p.json || strings.Contains(v.Text, "\n")
	p.ed.Focus()
	p.setWidth(maxWidth)
	p.ed.SelectAll()
	return p
}

// popupChrome is the popup's rows around the editor: borders, the blank
// rows and up to four rows of notes.
const popupChrome = 9

func (p *cellPopup) setWidth(maxWidth int) {
	p.maxWidth = maxWidth
	p.width = min(84, maxWidth)
	if p.multiline {
		p.width = min(max(84, maxWidth*4/5), maxWidth)
	}
	p.size()
}

// setHeight bounds the popup by the screen; multi-line values may take
// most of it.
func (p *cellPopup) setHeight(maxHeight int) {
	p.maxHeight = maxHeight
	p.size()
}

func (p *cellPopup) size() {
	limit := 12
	if p.multiline && p.maxHeight > 0 {
		limit = max(p.maxHeight*4/5-popupChrome, 3)
	}
	p.ed.SetSize(p.width-6, min(max(p.ed.LineCount(), 3), limit))
}

// prepareCell checks whether a cell can be edited and, if so, loads what
// editing needs on the tab's session.
func (m *Model) prepareCell(p *cellPopup) tea.Cmd {
	t, r := p.tab, p.run
	cfg := t.ctx.conn.cfg()
	switch {
	case cfg.ReadOnly:
		p.reason = "The connection is read-only."
	case r.busy():
		p.reason = "Rows are still arriving; esc in the results stops fetching, then the cell can be edited."
	case t.session == nil || t.sessionCtx != t.ctx || t.run != r:
		p.reason = "Run the query again to edit its results."
	case r.originErr != nil:
		p.reason = r.originErr.Error()
	}
	if p.reason != "" {
		return nil
	}

	p.phase = cellLoading
	session, origin, query := t.session, r.origin, r.query
	set, row, col := r.res.Rows, p.row, p.col
	driver := cfg.Driver
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), cellTimeout)
		defer cancel()
		msg := cellReadyMsg{popup: p}
		if origin == nil {
			var err error
			if origin, err = db.FindOrigin(ctx, session, driver, query, len(set.Columns())); err != nil {
				msg.originErr, msg.reason = err, reasonFor(err)
				return msg
			}
		}
		msg.origin = origin
		if err := origin.Editable(col); err != nil {
			msg.reason = err.Error()
			return msg
		}
		key, err := origin.KeyValues(func(c int) db.Value { return set.Cell(row, c) })
		if err != nil {
			msg.reason = err.Error()
			return msg
		}
		cell := &db.Cell{Origin: origin, Col: col, Key: key}
		current, err := cell.Current(ctx, session)
		if err != nil {
			msg.reason = reasonFor(err)
			return msg
		}
		msg.cell, msg.current = cell, current
		return msg
	}
}

// reasonFor turns a failure to prepare an edit into a sentence.
func reasonFor(err error) string {
	var ne db.NotEditable
	switch {
	case errors.As(err, &ne):
		return ne.Reason
	case errors.Is(err, db.ErrRowChanged):
		s := err.Error()
		return strings.ToUpper(s[:1]) + s[1:] + "."
	}
	return "Cannot edit: " + firstLine(db.Describe(err).Message)
}

// ready switches the popup to editing, or explains why not.
func (p *cellPopup) ready(msg cellReadyMsg) {
	if msg.reason != "" {
		p.phase, p.reason = cellViewing, msg.reason
		return
	}
	p.phase, p.cell, p.old = cellEditing, msg.cell, msg.current
	p.ed.ReadOnly = false
	if !p.json && msg.cell.Origin.IsJSON(p.col) {
		// A text column kept to JSON by an ISJSON check: like PostgreSQL's
		// json, highlighted and validated, stored as written.
		p.json, p.normalized, p.multiline = true, false, true
		p.ed.SetLanguage(editor.JSON)
		p.setWidth(p.maxWidth)
	}
	if msg.current == nil {
		p.ed.SetText("")
		p.null, p.nullAt = true, p.ed.Version()
		p.original = ""
	} else {
		// The database's own text: it may differ from the grid's display,
		// e.g. a time zone or all digits of a number.
		text := *msg.current
		if p.normalized {
			text = prettyJSON(text) // the database keeps its own form anyway
		}
		p.ed.SetText(text)
		p.original = text
		p.multiline = p.multiline || strings.Contains(text, "\n")
	}
	p.syncPlaceholder()
	p.size() // the text may have more lines now
	p.ed.SelectAll()
}

func (p *cellPopup) syncPlaceholder() {
	p.ed.Placeholder = ""
	if p.null {
		p.ed.Placeholder = "NULL"
	}
}

// value is the edited value; nil for NULL.
func (p *cellPopup) value() *string {
	if p.null {
		return nil
	}
	s := p.ed.Text()
	return &s
}

func (p *cellPopup) changed() bool {
	if p.null != (p.old == nil) {
		return true
	}
	return !p.null && p.ed.Text() != p.original
}

func (p *cellPopup) save() tea.Cmd {
	if !p.changed() {
		return closeModal
	}
	if p.json && !p.null {
		if msg, line, col, ok := jsonError(p.ed.Text()); !ok {
			p.err = msg
			p.ed.SetCursorPosition(line, col) // where the JSON goes wrong
			return nil
		}
	}
	p.phase, p.err = cellSaving, ""
	cell, old, value, session := *p.cell, p.old, p.value(), p.tab.session
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), cellTimeout)
		defer cancel()
		v, err := cell.Update(ctx, session, old, value)
		return cellSavedMsg{popup: p, value: v, err: err}
	}
}

func (p *cellPopup) Update(msg tea.Msg) (modal, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		if paste, ok := msg.(tea.PasteMsg); ok && p.phase == cellEditing {
			cmd := p.ed.Update(paste)
			p.afterEdit()
			return p, cmd
		}
		return p, nil
	}
	switch key.String() {
	case "esc":
		return p, closeModal
	case "ctrl+s":
		if p.phase == cellEditing {
			return p, p.save()
		}
		return p, nil
	case "ctrl+f":
		if p.json {
			p.format()
		}
		return p, nil
	case "enter":
		switch {
		case p.phase == cellEditing && p.multiline:
			// A new line, like alt+enter; ctrl+s saves.
		case p.phase == cellEditing:
			return p, p.save()
		case p.phase == cellSaving:
			return p, nil
		default:
			return p, closeModal
		}
		fallthrough
	case "alt+enter":
		if p.phase == cellEditing {
			cmd := p.ed.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			p.afterEdit()
			return p, cmd
		}
		return p, nil
	case "ctrl+n":
		if p.phase == cellEditing {
			p.null, p.err = true, ""
			p.ed.SetText("")
			p.nullAt = p.ed.Version()
			p.syncPlaceholder()
		}
		return p, nil
	}
	if p.phase == cellSaving {
		return p, nil
	}
	cmd := p.ed.Update(key)
	p.afterEdit()
	return p, cmd
}

// format pretty-prints the JSON being shown or edited.
func (p *cellPopup) format() {
	if p.null {
		return
	}
	text := p.ed.Text()
	if msg, line, col, ok := jsonError(text); !ok {
		p.err = msg
		p.ed.SetCursorPosition(line, col)
		return
	}
	if pretty := prettyJSON(text); pretty != text {
		readOnly := p.ed.ReadOnly
		p.ed.ReadOnly = false // formatting a value being viewed changes only the view
		p.ed.SelectAll()
		p.ed.Update(tea.PasteMsg{Content: pretty}) // one undo step
		p.ed.ReadOnly = readOnly
		p.ed.SetCursorPosition(0, 0)
		p.size()
	}
	p.err = ""
}

// afterEdit drops NULL once text is typed.
func (p *cellPopup) afterEdit() {
	if p.null && p.ed.Version() != p.nullAt {
		p.null = false
		p.syncPlaceholder()
	}
	p.err = ""
}

// cursor places the terminal cursor in the editor, relative to the popup.
func (p *cellPopup) cursor() (x, y int, ok bool) {
	if p.phase != cellEditing {
		return 0, 0, false
	}
	x, y, ok = p.ed.Cursor()
	return x + 3, y + 2, ok // border and padding; blank row above
}

func (p *cellPopup) View(int) string {
	inner := p.width - 6
	rows := []string{""}
	rows = append(rows, strings.Split(p.ed.View(), "\n")...)
	rows = append(rows, "")

	var note string
	switch p.phase {
	case cellLoading:
		note = mutedStyle.Italic(true).Render("Finding the row…")
	case cellSaving:
		note = mutedStyle.Italic(true).Render("Saving…")
	case cellViewing:
		if p.reason != "" {
			note = mutedStyle.Render(p.reason)
		}
	case cellEditing:
		if p.changed() {
			note = mutedStyle.Render(p.cell.Preview(p.value()))
		} else {
			note = mutedStyle.Render("Editing " + p.cell.Origin.Table + ".")
		}
	}
	if p.err != "" {
		note = errorStyle.Render(p.err)
	}
	if note != "" {
		rows = append(rows, wrapLines(note, inner, 4)...)
	}
	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))

	title := p.column.Name
	if p.column.Type != "" {
		title += " · " + strings.ToLower(p.column.Type)
	}
	footer := hints("^c", "copy", "esc", "close")
	if p.json {
		footer = hints("^f", "format", "^c", "copy", "esc", "close")
	}
	switch {
	case p.phase == cellEditing && p.json:
		footer = hints("^s", "save", "^f", "format", "^n", "NULL", "esc", "cancel")
	case p.phase == cellEditing && p.multiline:
		footer = hints("^s", "save", "^n", "NULL", "esc", "cancel")
	case p.phase == cellEditing:
		footer = hints("⏎", "save", "alt+⏎", "new line", "^n", "NULL", "esc", "cancel")
	}
	pn := pane{title: title, footer: footer}
	if p.null && p.phase == cellEditing {
		pn.corner = accentStyle.Render("NULL")
	}
	return pn.render(body, p.width, lipgloss.Height(body)+3, true)
}
