package ui

import (
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

func (p exportPrefs) save(path string) {
	if path == "" {
		return
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, append(data, '\n'), 0o600)
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

// nullChoices are how CSV and TSV may write NULL.
var nullChoices = []string{"", "NULL", `\N`}

type exportDialog struct {
	tab      *tab
	run      *run
	set      *db.RowSet
	setIndex int
	driver   db.Driver
	prefs    string // where the choices are remembered

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
	matches   []string

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
	d := &exportDialog{tab: t, run: r, set: r.set(), setIndex: r.cur, prefs: m.exportPrefsPath(),
		table: newInput(), file: newInput()}
	if t.sessionCtx.conn != nil {
		d.driver = t.sessionCtx.conn.cfg().Driver
	}
	d.sep, d.decimal = export.ListSeparator(), export.DecimalSeparator()
	prefs, ok := loadExportPrefs(d.prefs)
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
			if n == prefs.Null {
				d.null = i
			}
		}
		d.bom = prefs.BOM
	}

	d.table.SetValue("result")
	if v := r.view(); v.origin != nil {
		d.table.SetValue(v.origin.SQLTable())
	}
	dir := prefs.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	d.file.SetValue(tildePath(filepath.Join(dir, exportName(t, r)+d.format.Ext())))
	d.file.CursorEnd()
	d.field = efFormat
	d.setWidth(m.dialogWidth())
	d.refreshMatches()

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
	if msg.origin != nil && !d.tableSet {
		d.table.SetValue(msg.origin.SQLTable())
	}
	// The default file name follows the table, found only now.
	if msg.origin != nil && !d.fileSet {
		path := d.file.Value()
		dir := path[:lastSeparator(path)+1]
		d.file.SetValue(dir + exportName(d.tab, d.run) + d.format.Ext())
		d.file.CursorEnd()
		d.refreshMatches()
	}
}

// exportName is the file's default name: the result's table, else the tab.
func exportName(t *tab, r *run) string {
	if v := r.view(); v.origin != nil && v.origin.Table != "" {
		name := v.origin.Table
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		return strings.Trim(name, `"[]`+"`")
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
	return d.incomplete() && !d.clipboard && db.MayWrite(d.run.query) == "" &&
		t.session != nil && t.sessionCtx == t.ctx && t.run == d.run && !t.run.busy()
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

func (d *exportDialog) refreshMatches() {
	d.matches = pathMatches(d.file.Value(), func(string, string) bool { return true })
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
				if value, ok := completePath(d.file.Value(), d.matches); ok {
					d.file.SetValue(value)
					d.file.CursorEnd()
					d.refreshMatches()
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
		d.refreshMatches()
		d.status, d.overwrite = "", ""
	}
	return d, cmd
}

// options are the export's options as chosen.
func (d *exportDialog) options() export.Options {
	return export.Options{Format: d.format, Separator: d.sep, Decimal: d.decimal, Null: nullChoices[d.null],
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
	rows   int64
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
	path := absPath(strings.TrimSpace(d.file.Value()))
	if strings.TrimSpace(d.file.Value()) == "" {
		d.status = errorStyle.Render("Give the file a name.")
		return nil
	}
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
		tmp := path + ".tmp"
		if path == "" {
			out = &buf
		} else {
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				done.err = err
				return done
			}
			file, out = f, f
		}
		bw := &countingWriter{w: out}

		var w export.Writer
		row := func(values []db.Value) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if path == "" && bw.n > clipboardLimit {
				return errTooBigForClipboard
			}
			d.written.Add(1)
			return w.Row(values)
		}
		var err error
		if all {
			err = db.ExportQuery(ctx, session, driver, query, index, func(c []db.ExportColumn) error {
				var err error
				w, err = export.New(bw, o, c)
				return err
			}, row)
		} else {
			if w, err = export.New(bw, o, cols); err == nil {
				n := set.Len()
				values := make([]db.Value, len(cols))
				for i := 0; i < n && err == nil; i++ {
					for c := range cols {
						values[c] = set.Cell(i, c)
					}
					err = row(values)
				}
			}
		}
		if err == nil && w != nil {
			err = w.Close()
		}
		done.rows = d.written.Load()
		if file != nil {
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

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

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
	prefs := exportPrefs{Format: d.format.String(), Separator: string(d.sep), Null: nullChoices[d.null], BOM: d.bom}
	if msg.path != "" {
		prefs.Dir = filepath.Dir(msg.path)
	} else if old, ok := loadExportPrefs(d.prefs); ok {
		prefs.Dir = old.Dir
	}
	prefs.save(d.prefs)
	if m.modal == d {
		m.modal = nil
	}
	rows := fmt.Sprintf("%d rows", msg.rows)
	if msg.rows == 1 {
		rows = "1 row"
	}
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
	choices := func(f exportField, items []string, cur int) string {
		var out []string
		for i, it := range items {
			st := mutedStyle.Padding(0, 1)
			if i == cur {
				st = textStyle.Padding(0, 1)
				if f == d.field && !d.working {
					st = badgeFocusedStyle.Padding(0, 1)
				}
			}
			out = append(out, st.Render(it))
		}
		return strings.Join(out, "")
	}

	var rows []string
	n := d.set.Len()
	what := fmt.Sprintf("%d rows, %d columns", n, len(d.set.Columns()))
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
			cur := 0
			if d.sep == ',' {
				cur = 1
			}
			row := label(f, "Separator") + choices(f, []string{"; semicolon", ", comma"}, cur)
			// Spreadsheets of decimal-comma locales want their decimals so.
			if export.DecimalComma(d.options()) {
				row += mutedStyle.Render("  decimals 120,5")
			}
			rows = append(rows, row)
		case efNull:
			rows = append(rows, label(f, "NULL as")+choices(f, []string{"empty", "NULL", `\N`}, d.null))
		case efBOM:
			cur := 0
			if d.bom {
				cur = 1
			}
			rows = append(rows, label(f, "BOM")+choices(f, []string{"no", "yes, for Excel"}, cur))
		case efTable:
			rows = append(rows, label(f, "Table")+" "+d.table.View()) // in line with the choices
		case efRows:
			cur := 0
			if d.all {
				cur = 1
			}
			rows = append(rows, label(f, "Rows")+choices(f, []string{fmt.Sprintf("the %d here", n), "all: run it again"}, cur))
		case efTo:
			cur := 0
			if d.clipboard {
				cur = 1
			}
			rows = append(rows, label(f, "To")+choices(f, []string{"a file", "the clipboard"}, cur))
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
		rows = append(rows, mutedStyle.Render(""))
	}

	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))
	footer := hints("↑↓", "field", "←→", "change", "⏎", "export", "esc", "cancel")
	if d.working {
		footer = hints("esc", "stop")
	}
	p := pane{title: "Export", footer: footer}
	return p.render(body, d.width, lipgloss.Height(body)+2, true)
}
