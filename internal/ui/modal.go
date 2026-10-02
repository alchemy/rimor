package ui

import (
	"context"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// modal is a dialog drawn over the layout. While one is open it receives
// every key press.
type modal interface {
	Update(msg tea.Msg) (modal, tea.Cmd)
	View(maxWidth int) string
}

// resizer is implemented by modals that need to know the screen width.
type resizer interface{ setWidth(maxWidth int) }

// heightLimiter is implemented by modals whose content can outgrow the
// screen and must scroll.
type heightLimiter interface{ setHeight(maxHeight int) }

type closeModalMsg struct{}

// saveConnectionMsg carries a submitted connection form.
type saveConnectionMsg struct {
	conn *connection // nil for a new connection
	cfg  db.Config
}

type deleteConnectionMsg struct{ conn *connection }

func closeModal() tea.Msg { return closeModalMsg{} }

// ── Connection form ─────────────────────────────────────────────────────────

const (
	fieldName = iota
	fieldDriver
	fieldDSN
	fieldCount
)

const labelWidth = 9

type testResultMsg struct {
	gen int
	err error
}

type connForm struct {
	conn   *connection
	taken  []string // names already used by other connections
	width  int
	name   textinput.Model
	dsn    textinput.Model
	driver int
	field  int

	testing bool
	testGen int
	status  string // rendered result of the last test or validation
}

func newConnForm(conn *connection, names []string, maxWidth int) (*connForm, tea.Cmd) {
	f := &connForm{conn: conn, name: newInput(), dsn: newInput()}
	f.setWidth(maxWidth)
	f.name.Placeholder = "my-database"
	f.name.CharLimit = 64
	for _, n := range names {
		if conn == nil || n != conn.cfg().Name {
			f.taken = append(f.taken, n)
		}
	}
	if conn != nil {
		cfg := conn.cfg()
		f.name.SetValue(cfg.Name)
		f.dsn.SetValue(cfg.DSN)
		f.driver = max(slices.Index(db.Drivers, cfg.Driver), 0)
	}
	f.syncDriver()
	return f, f.focus(fieldName)
}

func newInput() textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	s := textinput.DefaultDarkStyles()
	s.Focused.Text = textStyle
	s.Focused.Placeholder = mutedStyle
	s.Blurred.Text = titleStyle
	s.Blurred.Placeholder = mutedStyle
	s.Cursor.Color = colorAccent
	in.SetStyles(s)
	return in
}

func (f *connForm) cfg() db.Config {
	return db.Config{
		Name:   strings.TrimSpace(f.name.Value()),
		Driver: db.Drivers[f.driver],
		DSN:    strings.TrimSpace(f.dsn.Value()),
	}
}

func (f *connForm) syncDriver() {
	f.dsn.Placeholder = db.Drivers[f.driver].DSNHint()
}

func (f *connForm) focus(field int) tea.Cmd {
	f.field = (field + fieldCount) % fieldCount
	f.name.Blur()
	f.dsn.Blur()
	switch f.field {
	case fieldName:
		return f.name.Focus()
	case fieldDSN:
		return f.dsn.Focus()
	}
	return nil
}

func (f *connForm) validate() bool {
	cfg := f.cfg()
	switch {
	case cfg.Name == "":
		f.status = errorStyle.Render("A name is required.")
		f.focus(fieldName)
	case slices.Contains(f.taken, cfg.Name):
		f.status = errorStyle.Render("Another connection already uses this name.")
		f.focus(fieldName)
	case cfg.DSN == "":
		f.status = errorStyle.Render(dsnLabel(cfg.Driver) + " is required.")
		f.focus(fieldDSN)
	default:
		return true
	}
	return false
}

func dsnLabel(d db.Driver) string {
	if d == db.SQLite {
		return "File"
	}
	return "DSN"
}

func (f *connForm) test() tea.Cmd {
	cfg := f.cfg()
	if cfg.DSN == "" {
		f.status = errorStyle.Render(dsnLabel(cfg.Driver) + " is required.")
		return nil
	}
	f.testGen++
	f.testing = true
	f.status = mutedStyle.Render("Connecting…")
	gen := f.testGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		conn, err := db.Open(ctx, cfg)
		if err == nil {
			conn.Close()
		}
		return testResultMsg{gen, err}
	}
}

func (f *connForm) Update(msg tea.Msg) (modal, tea.Cmd) {
	switch msg := msg.(type) {
	case testResultMsg:
		if msg.gen == f.testGen {
			f.testing = false
			if msg.err != nil {
				f.status = errorStyle.Render("✗ " + msg.err.Error())
			} else {
				f.status = okStyle.Render("✓ Connected")
			}
		}
		return f, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			return f, closeModal
		case "tab", "down":
			return f, f.focus(f.field + 1)
		case "shift+tab", "up":
			return f, f.focus(f.field - 1)
		case "ctrl+t":
			return f, f.test()
		case "ctrl+s":
			return f, f.submit()
		case "enter":
			if f.field == fieldDSN {
				return f, f.submit()
			}
			return f, f.focus(f.field + 1)
		}
		if f.field == fieldDriver {
			switch msg.String() {
			case "left", "h":
				f.driver = (f.driver + len(db.Drivers) - 1) % len(db.Drivers)
			case "right", "l", "space":
				f.driver = (f.driver + 1) % len(db.Drivers)
			}
			f.syncDriver()
			return f, nil
		}
		f.status = ""
	}

	// Everything else (typing, cursor blinks) goes to the inputs.
	var c1, c2 tea.Cmd
	f.name, c1 = f.name.Update(msg)
	f.dsn, c2 = f.dsn.Update(msg)
	return f, tea.Batch(c1, c2)
}

func (f *connForm) submit() tea.Cmd {
	if !f.validate() {
		return nil
	}
	msg := saveConnectionMsg{conn: f.conn, cfg: f.cfg()}
	return func() tea.Msg { return msg }
}

// setWidth sizes the form for a screen maxWidth cells wide. Inputs must be
// sized before they receive text so that long values scroll.
func (f *connForm) setWidth(maxWidth int) {
	f.width = min(72, maxWidth)
	inputW := f.inner() - labelWidth - 1 // the input draws one extra cell for its cursor
	f.name.SetWidth(inputW)
	f.dsn.SetWidth(inputW)
}

// inner is the content width: border and two cells of padding each side.
func (f *connForm) inner() int { return f.width - 6 }

func (f *connForm) View(int) string {
	inner := f.inner()

	label := func(field int, text string) string {
		st := mutedStyle
		if f.field == field {
			st = accentStyle.Bold(true)
		}
		return st.Width(labelWidth).Render(text)
	}

	var drivers []string
	for i, d := range db.Drivers {
		st := mutedStyle.Padding(0, 1)
		if i == f.driver {
			st = textStyle.Padding(0, 1)
			if f.field == fieldDriver {
				st = badgeFocusedStyle.Padding(0, 1)
			}
		}
		drivers = append(drivers, st.Render(d.Label()))
	}

	rows := []string{
		"",
		label(fieldName, "Name") + f.name.View(),
		"",
		label(fieldDriver, "Driver") + strings.Join(drivers, " "),
		"",
		label(fieldDSN, dsnLabel(db.Drivers[f.driver])) + f.dsn.View(),
		"",
	}
	if f.status != "" {
		rows = append(rows, wrapLines(f.status, inner, 3)...)
	}
	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))

	title := "New connection"
	if f.conn != nil {
		title = "Edit connection"
	}
	p := pane{title: title, footer: hints("^t", "test", "^s", "save", "esc", "cancel")}
	return p.render(body, f.width, lipgloss.Height(body)+3, true)
}

// ── Delete confirmation ─────────────────────────────────────────────────────

type confirmDelete struct{ conn *connection }

func (c confirmDelete) Update(msg tea.Msg) (modal, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "y", "enter":
			conn := c.conn
			return c, func() tea.Msg { return deleteConnectionMsg{conn} }
		case "n", "esc", "q":
			return c, closeModal
		}
	}
	return c, nil
}

func (c confirmDelete) View(maxWidth int) string {
	width := min(48, maxWidth)
	body := lipgloss.NewStyle().Padding(1, 2).Width(width - 2).Render(
		textStyle.Render("Delete ") + accentStyle.Bold(true).Render(c.conn.cfg().Name) +
			textStyle.Render("?") + "\n" +
			mutedStyle.Render("The saved connection is removed; the database is untouched."))
	p := pane{title: "Delete connection", footer: hints("y", "delete", "n", "keep")}
	return p.render(body, width, lipgloss.Height(body)+2, true)
}
