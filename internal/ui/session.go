package ui

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"rimor.dev/internal/paths"
)

// The session remembers open tabs across runs, including the text of
// unsaved ones, so quitting never loses work.

type sessionTab struct {
	Path       string  `json:"path,omitempty"`
	Name       string  `json:"name,omitempty"`
	Connection string  `json:"connection,omitempty"`
	Database   string  `json:"database,omitempty"`
	Content    *string `json:"content,omitempty"` // only for unsaved changes
	Line       int     `json:"line"`
	Col        int     `json:"col"`
}

type session struct {
	Tabs     []sessionTab   `json:"tabs"`
	Active   int            `json:"active"`
	Untitled int            `json:"untitled"`
	Layout   *sessionLayout `json:"layout,omitempty"`
}

// sessionLayout is where the pane borders were, as screen fractions.
type sessionLayout struct {
	Explorer float64 `json:"explorer"`
	Query    float64 `json:"query"`
}

// DefaultSessionPath is session.json in the state directory, e.g.
// ~/.local/state/rimor/session.json (see package paths).
func DefaultSessionPath() (string, error) {
	dir, err := paths.State()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "session.json"), nil
}

func loadSession(path string) (session, error) {
	var s session
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

func saveSession(path string, s session) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// snapshot captures the pane for the session file.
func (q *QueryPane) snapshot() session {
	s := session{Active: q.active, Untitled: q.untitled}
	for _, t := range q.tabs {
		st := sessionTab{Path: t.path, Database: t.ctx.database}
		if t.path == "" {
			st.Name = t.name
		}
		if t.ctx.conn != nil {
			st.Connection = t.ctx.conn.cfg().Name
		}
		if t.dirty() {
			text := t.ed.Text()
			st.Content = &text
		}
		st.Line, st.Col = t.ed.CursorPosition()
		s.Tabs = append(s.Tabs, st)
	}
	return s
}

// restore reopens the tabs of a session; resolve maps connection names to
// saved connections. Files that can no longer be read are reported in the
// notice and skipped, unless the session kept their unsaved text.
func (q *QueryPane) restore(s session, resolve func(string) *connection) {
	q.untitled = s.Untitled
	var missing []string
	for _, st := range s.Tabs {
		ctx := queryContext{database: st.Database}
		if st.Connection != "" {
			if ctx.conn = resolve(st.Connection); ctx.conn == nil {
				ctx.database = ""
			}
		}
		t := q.newTab(ctx)
		t.name, t.path = st.Name, st.Path

		if t.path != "" {
			data, err := os.ReadFile(t.path)
			if err != nil && st.Content == nil {
				missing = append(missing, filepath.Base(t.path))
				continue
			}
			t.ed.SetText(string(data))
			t.saved = t.ed.Version()
		}
		if st.Content != nil {
			t.ed.SetText(*st.Content)
			if t.path == "" && *st.Content == "" {
				t.saved = t.ed.Version()
			} else {
				t.saved = -1 // unsaved changes survive as unsaved
			}
		}
		t.ed.SetCursorPosition(st.Line, st.Col)
		q.tabs = append(q.tabs, t)
	}
	q.active = min(max(s.Active, 0), max(len(q.tabs)-1, 0))
	if len(missing) > 0 {
		q.notice = errorStyle.Render("Could not reopen " + strings.Join(missing, ", "))
	}
}
