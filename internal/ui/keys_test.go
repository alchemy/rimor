package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// handlerScopes maps each key handler to the key table that documents it.
var handlerScopes = []struct {
	file, fn string
	scope    keyScope
}{
	{"explorer.go", "*Explorer.handleKey", explorerKeys},
	{"results.go", "*ResultsPane.Update", resultsKeys},
	{"query.go", "*QueryPane.Update", tabKeys},
	{"cell.go", "*cellPopup.Update", cellKeys},
	{"modal.go", "*connForm.Update", connFormKeys},
	{"modal.go", "confirmDelete.Update", confirmDeleteKeys},
	{"dialogs.go", "*pathPrompt.Update", pathKeys},
	{"dialogs.go", "confirmCloseTab.Update", closeTabKeys},
	{"dialogs.go", "*contextPicker.Update", pickerKeys},
	{"help.go", "*helpOverlay.Update", helpKeys},
	{"model.go", "*Model.handleKey", paneKeys},
	{"../editor/editor.go", "*Model.handleKey", editorKeys},
	{"../editor/editor.go", "*Model.handleMove", editorKeys},
}

// handlerLiterals reads a function's string literals: those in case
// clauses (the keys it switches on) and all of them.
func handlerLiterals(t *testing.T, file, fn string) (cases, all []string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.FromSlash(file), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		decl, ok := d.(*ast.FuncDecl)
		if !ok || funcName(decl) != fn {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CaseClause:
				for _, e := range n.List {
					if s, ok := stringLit(e); ok {
						cases = append(cases, s)
					}
				}
			case *ast.BasicLit:
				if s, ok := stringLit(n); ok {
					all = append(all, s)
				}
			}
			return true
		})
		return cases, all
	}
	t.Fatalf("%s: no function %s", file, fn)
	return nil, nil
}

func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil {
		return d.Name.Name
	}
	switch r := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		return "*" + r.X.(*ast.Ident).Name + "." + d.Name.Name
	case *ast.Ident:
		return r.Name + "." + d.Name.Name
	}
	return d.Name.Name
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// TestKeyTablesMatchHandlers keeps the help honest: every key a handler
// switches on is in its table, and every key in a table is one its
// handlers mention.
func TestKeyTablesMatchHandlers(t *testing.T) {
	handled := map[string][]string{}    // table title → literals of its handlers
	documented := map[string]keyScope{} // table title → table
	for _, h := range handlerScopes {
		cases, all := handlerLiterals(t, h.file, h.fn)
		var keys []string
		for _, b := range h.scope.keys {
			keys = append(keys, b.keys...)
		}
		for _, k := range cases {
			if !slices.Contains(keys, k) {
				t.Errorf("%s %s handles %q, which the %q key table does not list", h.file, h.fn, k, h.scope.title)
			}
		}
		handled[h.scope.title] = append(handled[h.scope.title], all...)
		documented[h.scope.title] = h.scope
	}
	for title, scope := range documented {
		for _, b := range scope.keys {
			for _, k := range b.keys {
				if !slices.Contains(handled[title], k) {
					t.Errorf("the %q key table lists %q, which no handler of it mentions", title, k)
				}
			}
		}
	}
	// Every table is in the "all keys" view.
	for _, h := range handlerScopes {
		if !slices.ContainsFunc(allScopes, func(s keyScope) bool { return s.title == h.scope.title }) {
			t.Errorf("%q is missing from allScopes", h.scope.title)
		}
	}
}

func TestHelpOverlay(t *testing.T) {
	d := layoutDriver(t, "")
	d.key("f1")
	if d.m.help == nil {
		t.Fatal("F1 did not open the help")
	}
	s := ansi.Strip(d.m.render())
	t.Log("\n" + s)
	for _, want := range []string{"Keys · Explorer", "Add a connection", "Everywhere", "Quit", "^g", "tab all keys"} {
		if !strings.Contains(s, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if strings.Contains(s, "Copy the row") {
		t.Error("explorer help shows results keys")
	}

	// tab: every table; typing filters across them.
	d.key("tab")
	d.typeText("copy")
	s = ansi.Strip(d.m.help.View())
	for _, want := range []string{"Keys · all", "Copy the cell", "Copy the row", "Results", "Editor"} {
		if !strings.Contains(s, want) {
			t.Errorf("filtered help lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Add a connection") {
		t.Error("filter kept unrelated keys")
	}
	d.key("esc") // clears the filter
	if d.m.help == nil || d.m.help.filter != "" {
		t.Fatal("esc should clear the filter first")
	}
	d.key("esc")
	if d.m.help != nil {
		t.Fatal("esc did not close the help")
	}

	// ? opens it outside text input, scoped to the pane; in the editor
	// ? is text.
	d.key("alt+3", "?")
	if d.m.help == nil || d.m.help.place != "Results" {
		t.Fatalf("? in results: %+v", d.m.help)
	}
	d.key("f1") // F1 closes it again
	d.key("alt+2", "?")
	if d.m.help != nil || !strings.Contains(d.m.query.current().ed.Text(), "?") {
		t.Error("? in the editor should type")
	}
	d.key("f1")
	if d.m.help == nil || d.m.help.place != "Query" {
		t.Fatal("F1 in the editor")
	}
	if s := ansi.Strip(d.m.help.View()); !strings.Contains(s, "Undo") || !strings.Contains(s, "Run the selection") {
		t.Errorf("query help:\n%s", s)
	}
	d.key("esc")

	// Inside a dialog it describes the dialog.
	d.key("alt+1", "a", "f1")
	if d.m.help == nil || d.m.help.place != "Connection" || d.m.modal == nil {
		t.Fatalf("help over the form: help %+v modal %T", d.m.help, d.m.modal)
	}
	d.key("esc")
	if d.m.modal == nil {
		t.Error("closing the help closed the form too")
	}
	d.key("esc")

	// The leader opens it too.
	d.key("ctrl+g", "?")
	if d.m.help == nil {
		t.Error("ctrl+g ? did not open the help")
	}
	d.key("esc")
	d.screen()
}

func TestHelpShowsRemappedKeys(t *testing.T) {
	s := config.Defaults()
	s.Keys = map[string][]string{"full_screen": {"ctrl+z"}, "help": {"f2"}}
	d := &driver{t: t, m: New(db.Store{Path: filepath.Join(t.TempDir(), "c.json")}, "", s)}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	d.key("f1")
	if d.m.help != nil {
		t.Fatal("F1 still opens the help after remapping")
	}
	d.key("f2")
	if d.m.help == nil {
		t.Fatal("the remapped help key does not work")
	}
	if s := ansi.Strip(d.m.help.View()); !strings.Contains(s, "^z") || strings.Contains(s, "alt+f") {
		t.Errorf("help does not show the remapped binding:\n%s", s)
	}
}

func TestHelpConditionalKeys(t *testing.T) {
	d := layoutDriver(t, "")
	// The "." key shows only on a database; there is none here.
	d.key("f1")
	if strings.Contains(ansi.Strip(d.m.help.View()), "empty schemas") {
		t.Error("database-only key shown without a database")
	}
	d.key("tab") // the whole list, not just what fits on screen
	if !strings.Contains(ansi.Strip(strings.Join(d.m.help.lines(), "\n")), "empty schemas") {
		t.Error("all keys view hides conditional keys")
	}
}
