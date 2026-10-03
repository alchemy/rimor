package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func press(m *Model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		mods := strings.Split(k, "+")
		base := mods[len(mods)-1]
		for _, mod := range mods[:len(mods)-1] {
			switch mod {
			case "ctrl":
				msg.Mod |= tea.ModCtrl
			case "shift":
				msg.Mod |= tea.ModShift
			case "alt":
				msg.Mod |= tea.ModAlt
			}
		}
		switch base {
		case "enter":
			msg.Code = tea.KeyEnter
		case "tab":
			msg.Code = tea.KeyTab
		case "backspace":
			msg.Code = tea.KeyBackspace
		case "delete":
			msg.Code = tea.KeyDelete
		case "left":
			msg.Code = tea.KeyLeft
		case "right":
			msg.Code = tea.KeyRight
		case "up":
			msg.Code = tea.KeyUp
		case "down":
			msg.Code = tea.KeyDown
		case "home":
			msg.Code = tea.KeyHome
		case "end":
			msg.Code = tea.KeyEnd
		default:
			msg.Code = []rune(base)[0]
			if msg.Mod == 0 {
				msg.Text = base
			}
		}
		m.Update(msg)
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		if r == '\n' {
			press(m, "enter")
			continue
		}
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func newModel() *Model {
	m := New(Theme{})
	m.SetSize(40, 10)
	m.Focus()
	return m
}

func TestTypingAndAutoIndent(t *testing.T) {
	m := newModel()
	typeText(m, "SELECT *\n  FROM t\nWHERE")
	want := "SELECT *\n  FROM t\n  WHERE"
	if got := m.Text(); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestUndoGroupsTyping(t *testing.T) {
	m := newModel()
	typeText(m, "select")
	press(m, "enter")
	typeText(m, "from")
	press(m, "ctrl+z")
	if got := m.Text(); got != "select\n" {
		t.Fatalf("after one undo = %q", got)
	}
	press(m, "ctrl+z", "ctrl+z")
	if got := m.Text(); got != "" {
		t.Fatalf("after three undos = %q", got)
	}
	press(m, "ctrl+y", "ctrl+y", "ctrl+y")
	if got := m.Text(); got != "select\nfrom" {
		t.Fatalf("after redos = %q", got)
	}
}

func TestSelectionReplaceAndCut(t *testing.T) {
	m := newModel()
	m.SetText("SELECT name FROM users")
	press(m, "end")
	for range 5 {
		press(m, "shift+left")
	}
	if got := m.SelectedText(); got != "users" {
		t.Fatalf("selection = %q", got)
	}
	typeText(m, "people")
	if got := m.Text(); got != "SELECT name FROM people" {
		t.Fatalf("replace = %q", got)
	}
	press(m, "ctrl+a", "ctrl+x")
	if got := m.Text(); got != "" {
		t.Fatalf("cut all = %q", got)
	}
	press(m, "ctrl+v")
	if got := m.Text(); got != "SELECT name FROM people" {
		t.Fatalf("paste = %q", got)
	}
}

func TestIndentSelectedLines(t *testing.T) {
	m := newModel()
	m.SetText("a\nb\nc")
	press(m, "shift+down", "shift+down", "tab")
	if got := m.Text(); got != "    a\n    b\n    c" {
		t.Fatalf("indent = %q", got)
	}
	press(m, "shift+tab")
	if got := m.Text(); got != "a\nb\nc" {
		t.Fatalf("dedent = %q", got)
	}
}

func TestWordMovementAndDelete(t *testing.T) {
	m := newModel()
	m.SetText("select customer_id from orders")
	press(m, "ctrl+right", "ctrl+right")
	if _, col := m.CursorPosition(); col != len("select customer_id ") {
		t.Fatalf("col = %d", col)
	}
	press(m, "ctrl+backspace")
	if got := m.Text(); got != "select from orders" {
		t.Fatalf("word delete = %q", got)
	}
}

func TestBackspaceJoinsLines(t *testing.T) {
	m := newModel()
	m.SetText("ab\ncd")
	m.SetCursorPosition(1, 0)
	press(m, "backspace")
	if got := m.Text(); got != "abcd" {
		t.Fatalf("join = %q", got)
	}
}

func TestViewWidthAndScroll(t *testing.T) {
	m := newModel()
	m.SetText(strings.Repeat("x", 100) + "\n" + strings.Repeat("line\n", 30))
	press(m, "end")
	for _, row := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(row); w != 40 {
			t.Fatalf("row width %d: %q", w, ansi.Strip(row))
		}
	}
	if x, _, ok := m.Cursor(); !ok || x >= 40 {
		t.Fatalf("cursor x = %d ok=%v", x, ok)
	}
	m.SetCursorPosition(25, 0)
	if _, y, ok := m.Cursor(); !ok || y != 9 {
		t.Fatalf("cursor y = %d ok=%v, want the bottom row", y, ok)
	}
}

func TestHighlightTSQL(t *testing.T) {
	lines := [][]rune{
		[]rune("SELECT TOP 10 [Order Id], @limit -- note"),
		[]rune("FROM dbo.orders WHERE name = 'x'"),
	}
	cls := highlight(lexerFor(TSQL), lines)
	at := func(row int, word string) class {
		return cls[row][strings.Index(string(lines[row]), word)]
	}
	for _, c := range []struct {
		row  int
		word string
		want class
	}{
		{0, "SELECT", clsKeyword},
		{0, "10", clsNumber},
		{0, "@limit", clsVariable},
		{0, "-- note", clsComment},
		{1, "'x'", clsString},
		{1, "WHERE", clsKeyword},
	} {
		if got := at(c.row, c.word); got != c.want {
			t.Errorf("%q: class %d, want %d", c.word, got, c.want)
		}
	}
}

func TestHighlightMultilineComment(t *testing.T) {
	lines := [][]rune{[]rune("/* one"), []rune("two */ SELECT")}
	cls := highlight(lexerFor(PostgreSQL), lines)
	if cls[1][0] != clsComment || cls[1][7] != clsKeyword {
		t.Fatalf("classes = %v", cls[1])
	}
}

func TestReadOnly(t *testing.T) {
	m := newModel()
	m.SetText("keep me")
	m.ReadOnly = true
	typeText(m, "x")
	press(m, "backspace", "enter", "ctrl+v")
	m.Update(tea.PasteMsg{Content: "pasted"})
	if got := m.Text(); got != "keep me" {
		t.Fatalf("read-only text changed: %q", got)
	}
	press(m, "ctrl+a")
	if got := m.SelectedText(); got != "keep me" {
		t.Errorf("select all = %q", got)
	}
	m.NoLineNumbers = true
	if row := strings.Split(m.View(), "\n")[0]; !strings.HasPrefix(ansi.Strip(row), "keep me") {
		t.Errorf("gutter still shown: %q", ansi.Strip(row))
	}
}

func TestHighlightJSON(t *testing.T) {
	lines := [][]rune{[]rune(`{"name": "bolt", "price": 1.5,`), []rune(`  "tags": [true, null]}`)}
	cls := highlight(lexerFor(JSON), lines)
	at := func(row int, s string) class { return cls[row][strings.Index(string(lines[row]), s)] }
	for _, c := range []struct {
		row  int
		s    string
		want class
	}{
		{0, `"name"`, clsFunction}, {0, `"bolt"`, clsString}, {0, "1.5", clsNumber},
		{1, "true", clsKeyword}, {1, "null", clsKeyword}, {0, "{", clsPunctuation},
	} {
		if got := at(c.row, c.s); got != c.want {
			t.Errorf("%s: class %d, want %d", c.s, got, c.want)
		}
	}
}
