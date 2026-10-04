package ui

import (
	"cmp"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// driver feeds messages to a Model and runs the commands they produce.
// Animation messages (spinner ticks, cursor blinks) are dropped: each one
// schedules the next, so following them would never end.
type driver struct {
	t *testing.T
	m Model
	// wait is how long a command may run before it is taken for a timer
	// and dropped; raise it for tests against network servers.
	wait time.Duration
}

func (d *driver) send(msg tea.Msg) {
	next, cmd := d.m.Update(msg)
	d.m = next.(Model)
	d.run(cmd)
}

func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				d.run(c)
			}
			return
		}
		switch msg.(type) {
		case nil, spinner.TickMsg, cursor.BlinkMsg:
			return
		}
		if fmt.Sprintf("%T", msg) == "cursor.initialBlinkMsg" {
			return
		}
		d.send(msg)
	case <-time.After(cmp.Or(d.wait, 50*time.Millisecond)):
		// A timer; the test does not need it.
	}
}

// key sends key presses written like "enter", "a", "ctrl+s" or "alt+1".
func (d *driver) key(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		parts := strings.Split(k, "+")
		base := parts[len(parts)-1]
		if k == "+" {
			base, parts = "+", []string{"+"}
		}
		for _, mod := range parts[:len(parts)-1] {
			switch mod {
			case "ctrl":
				msg.Mod |= tea.ModCtrl
			case "alt":
				msg.Mod |= tea.ModAlt
			case "shift":
				msg.Mod |= tea.ModShift
			}
		}
		switch base {
		case "enter":
			msg.Code = tea.KeyEnter
		case "tab":
			msg.Code = tea.KeyTab
		case "esc":
			msg.Code = tea.KeyEscape
		case "up":
			msg.Code = tea.KeyUp
		case "down":
			msg.Code = tea.KeyDown
		case "left":
			msg.Code = tea.KeyLeft
		case "right":
			msg.Code = tea.KeyRight
		case "backspace":
			msg.Code = tea.KeyBackspace
		case "delete":
			msg.Code = tea.KeyDelete
		case "home":
			msg.Code = tea.KeyHome
		case "end":
			msg.Code = tea.KeyEnd
		case "pgup":
			msg.Code = tea.KeyPgUp
		case "pgdown":
			msg.Code = tea.KeyPgDown
		case "space":
			msg.Code, msg.Text = tea.KeySpace, " "
		case "f1", "f2", "f5":
			n, _ := strconv.Atoi(base[1:])
			msg.Code = tea.KeyF1 + rune(n-1)
		default:
			if len([]rune(base)) > 1 {
				d.t.Fatalf("key: unknown key name %q", base) // never guess a letter
			}
			msg.Code = []rune(base)[0]
			if msg.Mod == 0 {
				msg.Text = base
			}
		}
		d.send(msg)
	}
}

func (d *driver) typeText(s string) {
	for _, r := range s {
		d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// screen renders the model as plain text, failing if any row is not
// exactly the terminal width.
func (d *driver) screen() string {
	out := d.m.render()
	for i, row := range strings.Split(out, "\n") {
		if w := lipgloss.Width(row); w != d.m.width && d.m.modal == nil {
			d.t.Errorf("row %d is %d cells wide, want %d", i, w, d.m.width)
		}
	}
	return ansi.Strip(out)
}

func TestExplorerAddAndBrowseSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shop.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`CREATE TABLE customer (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	store := db.Store{Path: filepath.Join(dir, "connections.json")}
	d := &driver{t: t, m: New(store, "", config.Defaults())}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})

	if s := d.screen(); !strings.Contains(s, "New connection") || !strings.Contains(s, "No saved connections") {
		t.Fatalf("empty state missing:\n%s", s)
	}

	// Fill the form: name, driver (Postgres → SQLite), file.
	d.key("a")
	d.typeText("shop")
	d.key("tab", "l", "tab")
	d.typeText(path)
	if s := d.screen(); !strings.Contains(s, "New connection") || !strings.Contains(s, "File") {
		t.Fatalf("form not shown:\n%s", s)
	}
	d.key("ctrl+s")
	if d.m.modal != nil {
		t.Fatalf("form still open:\n%s", d.screen())
	}

	saved, err := store.Load()
	if err != nil || len(saved) != 1 || saved[0].Driver != db.SQLite || saved[0].DSN != path {
		t.Fatalf("saved = %+v, %v", saved, err)
	}

	// Connect, then open Tables → customer → Columns.
	d.key("enter", "down", "enter", "down", "enter", "down", "enter")
	s := d.screen()
	t.Log("\n" + s)
	for _, want := range []string{"shop", "Tables  1", "customer", "Columns  2", "id  INTEGER", "name  TEXT"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen lacks %q", want)
		}
	}

	// Delete it again.
	d.key("g", "down", "d", "y")
	if saved, _ := store.Load(); len(saved) != 0 {
		t.Fatalf("not deleted: %+v", saved)
	}
	d.m.Close()
}

func TestExplorerConnectError(t *testing.T) {
	dir := t.TempDir()
	store := db.Store{Path: filepath.Join(dir, "connections.json")}
	if err := store.Save([]db.Config{{Name: "gone", Driver: db.SQLite, DSN: filepath.Join(dir, "missing.db")}}); err != nil {
		t.Fatal(err)
	}
	d := &driver{t: t, m: New(store, "", config.Defaults())}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	d.key("down", "enter")
	s := d.screen()
	t.Log("\n" + s)
	if !strings.Contains(s, "✗") || !strings.Contains(s, "no such file") {
		t.Errorf("error not shown")
	}
}
