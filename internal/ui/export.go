package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
	"rimor.dev/internal/export"
)

// Exporting a result (e in the results pane): a dialog picks the format,
// its options and where it goes, a file or the clipboard. It writes the
// rows that arrived; when fetching stopped early it can instead run the
// statement again and stream every row into the file (see
// db.ExportQuery). The choices are remembered in export.json beside the
// session.

// clipboardLimit is about what terminals pass through OSC 52.
const clipboardLimit = 1 << 20

// exportPrefs are the choices remembered between exports.
type exportPrefs struct {
	Format    string `json:"format"`
	Separator string `json:"separator,omitempty"`
	Null      string `json:"null"`
	BOM       bool   `json:"bom,omitempty"`
	Dir       string `json:"dir,omitempty"`
}

func loadExportPrefs(path string) (exportPrefs, bool) {
	var p exportPrefs
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &p) != nil {
		return exportPrefs{}, false
	}
	return p, true
}

func (p exportPrefs) save(path string) error {
	if path == "" {
		return nil
	}
	return saveJSON(path, p)
}

// exportPrefsPath is export.json beside the session, or "" when the
// session is not kept (one-shot sessions, tests).
func (m *Model) exportPrefsPath() string {
	if m.sessionPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(m.sessionPath), "export.json")
}

type exportField int

const (
	efFormat exportField = iota
	efSeparator
	efNull
	efBOM
	efTable
	efRows
	efTo
	efFile
)

// nullChoices are how CSV and TSV may write NULL, and how the dialog
// names them.
var nullChoices = []struct{ value, label string }{{"", "empty"}, {"NULL", "NULL"}, {`\N`, `\N`}}

type exportDialog struct {
	tab      *tab
	run      *run
	set      *db.RowSet
	setIndex int
	driver   db.Driver
	writes   bool   // the statement may write: it is never run again
	dir      string // the folder last exported to, remembered

	format    export.Format
	sep       rune // the locale's, or the other one
	decimal   rune // the locale's decimal separator
	null      int  // index in nullChoices
	bom       bool
	table     textinput.Model
	tableSet  bool // the user typed a table name
	fileSet   bool // the user typed in the file name
	all       bool // run again and write every row
	clipboard bool
	file      textinput.Model

	field exportField
	width int

	status    string
	overwrite string // the path the user was warned exists
	working   bool
	written   atomic.Int64
	cancel    context.CancelFunc
}

// exportRequestMsg asks for the dialog on the result shown.
type exportRequestMsg struct{ tab *tab }

// newExportDialog opens the dialog with the remembered choices.
func (m *Model) newExportDialog(t *tab) (*exportDialog, tea.Cmd) {
	r := t.run
	d := &exportDialog{tab: t, run: r, set: r.set(), setIndex: r.cur, writes: db.MayWrite(r.query) != "",
		table: newInput(), file: newInput()}
	if t.sessionCtx.conn != nil {
		d.driver = t.sessionCtx.conn.cfg().Driver
	}
	d.sep, d.decimal = export.ListSeparator(), export.DecimalSeparator()
	prefs, ok := loadExportPrefs(m.exportPrefsPath())
	if ok {
		for _, f := range export.Formats {
			if f.String() == prefs.Format {
				d.format = f
			}
		}
		if r := []rune(prefs.Separator); len(r) == 1 {
			d.sep = r[0]
		}
		for i, n := range nullChoices {
			if n.value == prefs.Null {
				d.null = i
			}
		}
		d.bom = prefs.BOM
	}

	// The table defaults to the result's own, else to the writer's "result".
	d.table.Placeholder = "result"
	if v := r.view(); v.origin != nil {
		d.table.SetValue(v.origin.SQLTable())
	}
	d.dir = prefs.Dir
	dir := d.dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	d.file.SetValue(tildePath(filepath.Join(dir, exportName(t, r)+d.format.Ext())))
	d.file.CursorEnd()
	d.setWidth(m.dialogWidth())

	// The result's table, for INSERTs: found as editing finds it.
	if v := r.view(); v.origin == nil && v.originErr == nil && d.setIndex == 0 &&
		t.session != nil && t.sessionCtx == t.ctx && !t.busy() && d.set != nil {
		session, query, driver, cols := t.session, r.query, d.driver, len(d.set.Columns())
		return d, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), cellTimeout)
			defer cancel()
			origin, err := db.FindOrigin(ctx, session, driver, query, cols)
			return exportOriginMsg{d, origin, err}
		}
	}
	return d, nil
}

type exportOriginMsg struct {
	dialog *exportDialog
	origin *db.Origin
	err    error
}

func (m *Model) exportOrigin(msg exportOriginMsg) {
	d := msg.dialog
	v := d.run.view()
	if v.origin == nil && v.originErr == nil {
		v.origin, v.originErr = msg.origin, msg.err
	}
	if msg.origin == nil {
		return
	}
	if !d.tableSet {
		d.table.SetValue(msg.origin.SQLTable())
	}
	// The default file name follows the table, found only now.
	if !d.fileSet {
		path := d.file.Value()
		d.file.SetValue(path[:lastSeparator(path)+1] + exportName(d.tab, d.run) + d.format.Ext())
		d.file.CursorEnd()
	}
}

// exportName is the file's default name: the result's table, else the tab.
func exportName(t *tab, r *run) string {
	if v := r.view(); v.origin != nil && v.origin.Name() != "" {
		return v.origin.Name()
	}
	return strings.TrimSuffix(t.title(), ".sql")
}

func (d *exportDialog) setWidth(maxWidth int) {
	d.width = min(72, maxWidth)
	// Borders, padding, the label column and the cursor's cell.
	d.table.SetWidth(d.width - 2 - 4 - 12 - 3)
	d.file.SetWidth(d.width - 2 - 4 - 12 - 3)
}

// incomplete reports that not every row of the result is here.
func (d *exportDialog) incomplete() bool {
	p := d.set.Progress()
	return !p.Done || p.Stopped != db.StopNone
}

// canRunAgain reports whether the statement may run again into the file:
// not every row is here, it writes nothing, and the tab's session is idle.
func (d *exportDialog) canRunAgain() bool {
	t := d.tab
	return d.incomplete() && !d.clipboard && !d.writes &&
		t.session != nil && t.sessionCtx == t.ctx && t.run == d.run && !t.busy()
}

// fields are the fields the dialog shows now.
func (d *exportDialog) fields() []exportField {
	fs := []exportField{efFormat}
	switch d.format {
	case export.CSV:
		fs = append(fs, efSeparator, efNull, efBOM)
	case export.TSV:
		fs = append(fs, efNull)
	case export.SQL:
		fs = append(fs, efTable)
	}
	if d.canRunAgain() || (d.all && !d.clipboard) {
		fs = append(fs, efRows)
	}
	fs = append(fs, efTo)
	if !d.clipboard {
		fs = append(fs, efFile)
	}
	return fs
}

func (d *exportDialog) move(delta int) {
	fs := d.fields()
	i := 0
	for j, f := range fs {
		if f == d.field {
			i = j
		}
	}
	d.field = fs[(i+delta+len(fs))%len(fs)]
	d.table.Blur()
	d.file.Blur()
	switch d.field {
	case efTable:
		d.table.Focus()
	case efFile:
		d.file.Focus()
	}
}

// choose changes the choice under the cursor.
func (d *exportDialog) choose(delta int) {
	switch d.field {
	case efFormat:
		n := len(export.Formats)
		d.setFormat(export.Formats[(int(d.format)+delta+n)%n])
	case efSeparator:
		if d.sep == ';' {
			d.sep = ','
		} else {
			d.sep = ';'
		}
	case efNull:
		n := len(nullChoices)
		d.null = (d.null + delta + n) % n
	case efBOM:
		d.bom = !d.bom
	case efRows:
		d.all = !d.all
	case efTo:
		d.clipboard = !d.clipboard
		if d.clipboard {
			d.all = false
		}
	}
	d.status, d.overwrite = "", ""
}

// setFormat changes the format and the file's extension with it.
func (d *exportDialog) setFormat(f export.Format) {
	d.format = f
	path := d.file.Value()
	if ext := filepath.Ext(path); ext == "" || export.IsExt(ext) {
		d.file.SetValue(strings.TrimSuffix(path, ext) + f.Ext())
		d.file.CursorEnd()
	}
}

func (d *exportDialog) Update(msg tea.Msg) (modal, tea.Cmd) {
	switch msg := msg.(type) {
	case exportTickMsg:
		if msg.dialog == d && d.working {
			return d, exportTick(d)
		}
		return d, nil
	case tea.PasteMsg:
		return d.toInput(msg)
	case tea.KeyPressMsg:
		if d.working {
			if msg.String() == "esc" && d.cancel != nil {
				d.cancel()
			}
			return d, nil
		}
		switch msg.String() {
		case "esc":
			return d, closeModal
		case "enter":
			return d, d.start()
		case "up", "shift+tab":
			d.move(-1)
			return d, nil
		case "down":
			d.move(1)
			return d, nil
		case "tab":
			if d.field == efFile {
				matches := pathMatches(d.file.Value(), func(string, string) bool { return true })
				if value, ok := completePath(d.file.Value(), matches); ok {
					d.file.SetValue(value)
					d.file.CursorEnd()
				}
				return d, nil
			}
			d.move(1)
			return d, nil
		case "left", "right", "space":
			if d.field != efTable && d.field != efFile {
				delta := 1
				if msg.String() == "left" {
					delta = -1
				}
				d.choose(delta)
				return d, nil
			}
		}
		return d.toInput(msg)
	}
	return d, nil
}

// toInput gives a key or a paste to the text field under the cursor.
func (d *exportDialog) toInput(msg tea.Msg) (modal, tea.Cmd) {
	var cmd tea.Cmd
	switch d.field {
	case efTable:
		d.table, cmd = d.table.Update(msg)
		d.tableSet = true
	case efFile:
		d.file, cmd = d.file.Update(msg)
		d.fileSet = true
		d.status, d.overwrite = "", ""
	}
	return d, cmd
}

// options are the export's options as chosen.
func (d *exportDialog) options() export.Options {
	return export.Options{Format: d.format, Separator: d.sep, Decimal: d.decimal, Null: nullChoices[d.null].value,
		BOM: d.bom, Table: strings.TrimSpace(d.table.Value()), Driver: d.driver}
}

// exportTickMsg refreshes the count while a long export runs.
type exportTickMsg struct{ dialog *exportDialog }

func exportTick(d *exportDialog) tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return exportTickMsg{d} })
}

// exportDoneMsg reports how an export went.
type exportDoneMsg struct {
	dialog *exportDialog
	path   string // "" for the clipboard
	text   string // for the clipboard
	err    error
}

// start checks the destination and writes in the background.
func (d *exportDialog) start() tea.Cmd {
	o := d.options()

	if d.clipboard {
		d.working = true
		return d.writeTo(o, "")
	}
	name := strings.TrimSpace(d.file.Value())
	if name == "" {
		d.status = errorStyle.Render("Give the file a name.")
		return nil
	}
	path := absPath(name)
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		d.status = errorStyle.Render("That is a folder; add a file name.")
		return nil
	case err == nil && d.overwrite != path:
		d.overwrite = path
		d.status = errorStyle.Render(filepath.Base(path) + " exists: enter again to replace it.")
		return nil
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		d.status = errorStyle.Render("No such folder: " + tildePath(filepath.Dir(path)))
		return nil
	}
	d.working = true
	if d.all {
		d.tab.exporting = true
	}
	return tea.Batch(d.writeTo(o, path), exportTick(d))
}

// writeTo writes the export: the rows here, or, with all, every row of
// the statement run again; to path, or to the clipboard when path is "".
func (d *exportDialog) writeTo(o export.Options, path string) tea.Cmd {
	set, all, driver := d.set, d.all, d.driver
	session, query, index := d.tab.session, d.run.query, d.setIndex
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	cols := make([]db.ExportColumn, len(set.Columns()))
	for i, c := range set.Columns() {
		cols[i] = db.ExportColumn{Name: c.Name, Type: c.Type, Numeric: set.Numeric(i)}
	}

	return func() tea.Msg {
		defer cancel()
		done := exportDoneMsg{dialog: d, path: path}
		var out io.Writer
		var buf bytes.Buffer
		var file *os.File
		var fw *bufio.Writer
		tmp := path + ".tmp"
		if path == "" {
			out = &buf
		} else {
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				done.err = err
				return done
			}
			file, fw = f, bufio.NewWriter(f)
			out = fw
		}

		var w export.Writer
		row := func(values []db.Value) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if path == "" && buf.Len() > clipboardLimit {
				return errTooBigForClipboard
			}
			d.written.Add(1)
			return w.Row(values)
		}
		var err error
		if all {
			err = db.ExportQuery(ctx, session, driver, query, index, func(c []db.ExportColumn) error {
				var err error
				w, err = export.New(out, o, c)
				return err
			}, row)
		} else {
			if w, err = export.New(out, o, cols); err == nil {
				n := set.Len()
				values := make([]db.Value, len(cols))
				for i := 0; i < n && err == nil; i++ {
					set.Row(i, values)
					err = row(values)
				}
			}
		}
		if err == nil && w != nil {
			err = w.Close()
		}
		if file != nil {
			if err == nil {
				err = fw.Flush()
			}
			if cerr := file.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = os.Rename(tmp, path)
			}
			if err != nil {
				os.Remove(tmp)
			}
		}
		if path == "" && err == nil && buf.Len() > clipboardLimit {
			err = errTooBigForClipboard
		}
		done.err, done.text = err, buf.String()
		return done
	}
}

var errTooBigForClipboard = fmt.Errorf("too large for the clipboard, which terminals limit to about %d MB: export to a file", clipboardLimit>>20)

// exportDone closes the dialog with a note, or shows why it failed.
func (m *Model) exportDone(msg exportDoneMsg) tea.Cmd {
	d := msg.dialog
	d.working = false
	d.tab.exporting = false
	if msg.err != nil {
		d.written.Store(0)
		switch {
		case errors.Is(msg.err, context.Canceled):
			d.status = mutedStyle.Render("Stopped: nothing was written.")
		default:
			d.status = errorStyle.Render(firstLine(db.Describe(msg.err).Message))
		}
		return nil
	}
	if msg.path != "" {
		d.dir = filepath.Dir(msg.path)
	}
	prefs := exportPrefs{Format: d.format.String(), Separator: string(d.sep), Null: nullChoices[d.null].value, BOM: d.bom, Dir: d.dir}
	prefs.save(m.exportPrefsPath())
	if m.modal == d {
		m.modal = nil
	}
	rows := plural(d.written.Load(), "row")
	if msg.path == "" {
		m.results.notice = okStyle.Render("✓ Copied " + rows + " as " + d.format.String())
		return tea.SetClipboard(msg.text)
	}
	// The file's name only: a long path would push the note out of the
	// footer, and the folder is the one just chosen.
	m.results.notice = okStyle.Render("✓ Exported " + rows + " to " + filepath.Base(msg.path))
	return nil
}

func (d *exportDialog) View(int) string {
	label := func(f exportField, text string) string {
		st := mutedStyle
		if f == d.field && !d.working {
			st = accentStyle
		}
		return st.Width(12).Render(text)
	}
	choices := func(f exportField, options []string, chosen int) string {
		return choice(options, chosen, f == d.field && !d.working)
	}

	var rows []string
	n := d.set.Len()
	what := plural(int64(n), "row") + ", " + plural(int64(len(d.set.Columns())), "column")
	rows = append(rows, "", textStyle.Render(what))
	if d.incomplete() && !d.all {
		p := d.set.Progress()
		why := "rows are still arriving"
		switch p.Stopped {
		case db.StopLimit:
			why = "fetching stopped at the memory limit"
		case db.StopCancelled:
			why = "fetching was stopped"
		}
		rows = append(rows, lipgloss.NewStyle().Foreground(colorPeach).Render("Not every row is here: "+why+"."))
	}
	rows = append(rows, "")

	for _, f := range d.fields() {
		switch f {
		case efFormat:
			names := make([]string, len(export.Formats))
			for i, x := range export.Formats {
				names[i] = x.String()
			}
			rows = append(rows, label(f, "Format")+choices(f, names, int(d.format)))
		case efSeparator:
			row := label(f, "Separator") + choices(f, []string{"; semicolon", ", comma"}, b2i(d.sep == ','))
			// Spreadsheets of decimal-comma locales want their decimals so.
			if export.DecimalComma(d.options()) {
				row += mutedStyle.Render("  decimals 120,5")
			}
			rows = append(rows, row)
		case efNull:
			names := make([]string, len(nullChoices))
			for i, c := range nullChoices {
				names[i] = c.label
			}
			rows = append(rows, label(f, "NULL as")+choices(f, names, d.null))
		case efBOM:
			rows = append(rows, label(f, "BOM")+choices(f, []string{"no", "yes, for Excel"}, b2i(d.bom)))
		case efTable:
			rows = append(rows, label(f, "Table")+" "+d.table.View()) // in line with the choices
		case efRows:
			rows = append(rows, label(f, "Rows")+choices(f, []string{"the " + groupDigits(int64(n)) + " here", "all: run it again"}, b2i(d.all)))
		case efTo:
			rows = append(rows, label(f, "To")+choices(f, []string{"a file", "the clipboard"}, b2i(d.clipboard)))
		case efFile:
			rows = append(rows, label(f, "File")+" "+d.file.View())
		}
	}
	rows = append(rows, "")
	switch {
	case d.working:
		rows = append(rows, accentStyle.Render(fmt.Sprintf("Exporting… %d rows", d.written.Load())))
	case d.status != "":
		rows = append(rows, d.status)
	case d.all:
		rows = append(rows, mutedStyle.Render("The statement runs again on this tab's session; every row goes to the file."))
	default:
		rows = append(rows, "")
	}

	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))
	footer := hints("↑↓", "field", "←→", "change", "⏎", "export", "esc", "cancel")
	if d.working {
		footer = hints("esc", "stop")
	}
	p := pane{title: "Export", footer: footer}
	return p.render(body, d.width, lipgloss.Height(body)+2, true)
}
