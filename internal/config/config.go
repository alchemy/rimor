// Package config reads rimor's settings file, config.toml, writing a
// commented default one on first run.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Icon modes.
const (
	IconsAuto  = "auto"  // Nerd Font glyphs when the terminal likely has them
	IconsNerd  = "nerd"  // always Nerd Font glyphs
	IconsPlain = "plain" // Unicode symbols that every font has
)

// Themes.
const (
	ThemeAuto     = "auto"     // the terminal's colours on Omarchy, else dark or light by background
	ThemeDark     = "dark"     // Catppuccin Mocha
	ThemeLight    = "light"    // Catppuccin Latte
	ThemeTerminal = "terminal" // the terminal's 16 ANSI colours
)

// Settings is the content of config.toml.
type Settings struct {
	Theme string `toml:"theme"`
	Icons string `toml:"icons"`
	// Leader is pressed before a second key, as an alternative to the alt
	// shortcuts for terminals that keep alt for themselves.
	Leader string `toml:"leader"`
	// ResultMemoryMB caps the memory a result's rows take; fetching stops
	// there, keeping the rows so far.
	ResultMemoryMB int `toml:"result_memory_mb"`
	// Keys replaces the bindings of the named actions.
	Keys map[string][]string `toml:"keys"`
}

// Action is an app-wide command that can be bound to keys.
type Action struct {
	Name        string
	Description string
	Defaults    []string // "leader x" means the leader key, then x
}

// Actions lists every bindable action with its default keys. Terminals
// without the kitty keyboard protocol report shift+alt+digit as alt plus
// the shifted character, which depends on the layout, so the defaults
// include the US (!@#) and Italian (!"£) variants.
var Actions = []Action{
	{"quit", "Quit", []string{"ctrl+q"}},
	{"help", "Show the keys that apply here", []string{"f1", "leader ?"}},
	{"focus_explorer", "Focus the explorer", []string{"alt+1", "leader 1"}},
	{"focus_query", "Focus the query editor", []string{"alt+2", "leader 2"}},
	{"focus_results", "Focus the results", []string{"alt+3", "leader 3"}},
	{"full_screen", "Toggle full screen for the focused pane", []string{"alt+f", "leader f"}},
	{"full_screen_explorer", "Explorer in full screen", []string{"alt+shift+1", "alt+!", "leader shift+1", "leader !"}},
	{"full_screen_query", "Query editor in full screen", []string{"alt+shift+2", "alt+@", `alt+"`, "leader shift+2", "leader @", `leader "`}},
	{"full_screen_results", "Results in full screen", []string{"alt+shift+3", "alt+#", "alt+£", "leader shift+3", "leader #", "leader £"}},
	{"run", "Run the selection, or the whole tab", []string{"ctrl+enter", "f5", "alt+enter", "leader enter"}},
	{"new_tab", "New query tab", []string{"ctrl+t", "leader t"}},
	{"open_file", "Open a .sql file", []string{"ctrl+o", "leader o"}},
	{"grow_explorer", "Widen the explorer", []string{"alt+shift+right", "leader right", "leader l"}},
	{"shrink_explorer", "Narrow the explorer", []string{"alt+shift+left", "leader left", "leader h"}},
	{"grow_query", "Make the query editor taller", []string{"alt+shift+down", "leader down", "leader j"}},
	{"shrink_query", "Make the query editor shorter", []string{"alt+shift+up", "leader up", "leader k"}},
	{"reset_layout", "Reset pane sizes", []string{"alt+=", "leader ="}},
}

// Defaults are the settings used for anything config.toml leaves out.
func Defaults() Settings {
	return Settings{Theme: ThemeAuto, Icons: IconsAuto, Leader: "ctrl+g", ResultMemoryMB: 512}
}

// Bindings returns the keys of every action: the defaults, replaced by
// config.toml where it binds an action.
func (s Settings) Bindings() map[string][]string {
	out := make(map[string][]string, len(Actions))
	for _, a := range Actions {
		out[a.Name] = a.Defaults
		if keys, ok := s.Keys[a.Name]; ok {
			out[a.Name] = keys
		}
	}
	return out
}

// Load reads the settings file, creating it with commented defaults when
// it does not exist.
func Load(path string) (Settings, error) {
	s := Defaults()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return s, err
		}
		return s, os.WriteFile(path, []byte(Template()), 0o600)
	}

	md, err := toml.DecodeFile(path, &s)
	if err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return s, fmt.Errorf("%s: unknown setting %q", path, undecoded[0].String())
	}
	if err := s.validate(); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s Settings) validate() error {
	switch s.Theme {
	case ThemeAuto, ThemeDark, ThemeLight, ThemeTerminal:
	default:
		return fmt.Errorf("theme must be %q, %q, %q or %q, not %q", ThemeAuto, ThemeDark, ThemeLight, ThemeTerminal, s.Theme)
	}
	switch s.Icons {
	case IconsAuto, IconsNerd, IconsPlain:
	default:
		return fmt.Errorf("icons must be %q, %q or %q, not %q", IconsAuto, IconsNerd, IconsPlain, s.Icons)
	}
	if s.ResultMemoryMB < 1 {
		return fmt.Errorf("result_memory_mb must be at least 1, not %d", s.ResultMemoryMB)
	}
	if strings.TrimSpace(s.Leader) == "" || strings.Contains(s.Leader, " ") {
		return fmt.Errorf("leader must be a single key such as \"ctrl+g\", not %q", s.Leader)
	}

	known := map[string]bool{}
	for _, a := range Actions {
		known[a.Name] = true
	}
	for name := range s.Keys {
		if !known[name] {
			return fmt.Errorf("[keys]: unknown action %q", name)
		}
	}

	// Each key may trigger one action only; check in a fixed order so the
	// message is stable.
	owner := map[string]string{} // key → action
	bindings := s.Bindings()
	for _, a := range Actions {
		name := a.Name
		for _, key := range bindings[name] {
			key = Normalize(key)
			if key == "" {
				return fmt.Errorf("[keys] %s: empty key", name)
			}
			if key == s.Leader {
				return fmt.Errorf("[keys] %s: %q is the leader key", name, key)
			}
			if other, taken := owner[key]; taken && other != name {
				return fmt.Errorf("[keys]: %q is bound to both %s and %s", key, other, name)
			}
			owner[key] = name
		}
	}
	return nil
}

// Normalize trims a binding and collapses the space in "leader x".
func Normalize(key string) string {
	fields := strings.Fields(key)
	if len(fields) == 2 && fields[0] == "leader" {
		return "leader " + fields[1]
	}
	return strings.Join(fields, "")
}

// Template is the commented settings file written on first run. Every line
// shows the default, commented out.
func Template() string {
	d := Defaults()
	var b strings.Builder
	b.WriteString(`# rimor settings. Lines starting with # show the defaults; remove the #
# and change the value to override one.

# Colour theme:
#   "auto"     on Omarchy, "terminal", so rimor follows the system theme;
#              elsewhere "dark" or "light" to match the terminal's background
#   "dark"     Catppuccin Mocha
#   "light"    Catppuccin Latte
#   "terminal" the terminal's own 16 colours
# Setting NO_COLOR in the environment turns colours off whatever this says.
`)
	fmt.Fprintf(&b, "# theme = %q\n\n", d.Theme)
	b.WriteString(`# Icons in the explorer and tabs:
#   "auto"  Nerd Font glyphs when the terminal or system likely has a Nerd
#           Font (kitty, Ghostty and WezTerm bundle one), otherwise plain
#   "nerd"  always Nerd Font glyphs
#   "plain" Unicode symbols that every font has
`)
	fmt.Fprintf(&b, "# icons = %q\n\n", d.Icons)
	b.WriteString(`# The leader key, pressed before a second key, gives every alt shortcut an
# alternative for terminals that keep alt for themselves (macOS Terminal and
# iTerm2 by default, Windows Terminal for alt+enter and alt+shift+arrows).
# Press it to see the keys it offers.
`)
	fmt.Fprintf(&b, "# leader = %q\n\n", d.Leader)
	b.WriteString(`# Memory a query's rows may take, in MB. Rows arrive in the background
# and fetching stops at this limit, keeping what arrived; esc stops sooner.
`)
	fmt.Fprintf(&b, "# result_memory_mb = %d\n\n", d.ResultMemoryMB)
	b.WriteString(`# App-wide shortcuts. Binding an action replaces all of its default keys.
# "leader x" means the leader key, then x. Keys inside panes (the editor,
# the explorer, the results grid) are not remappable yet.
[keys]
`)
	for _, a := range Actions {
		quoted := make([]string, len(a.Defaults))
		for i, k := range a.Defaults {
			quoted[i] = tomlString(k)
		}
		fmt.Fprintf(&b, "# %s\n# %s = [%s]\n", a.Description, a.Name, strings.Join(quoted, ", "))
	}
	return b.String()
}

// tomlString quotes a key for TOML, preferring literal strings so that
// keys such as alt+" stay readable.
func tomlString(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return fmt.Sprintf("%q", s)
}
