package ui

import (
	"strings"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
)

// keymap resolves app-wide shortcuts, configured in config.toml.
type keymap struct {
	leader string
	direct map[string]string // key → action
	chord  map[string]string // key after the leader → action
	// chordKeys lists each action's leader keys, for the help pop-up.
	chordKeys map[string][]string
}

func newKeymap(s config.Settings) keymap {
	k := keymap{
		leader:    s.Leader,
		direct:    map[string]string{},
		chord:     map[string]string{},
		chordKeys: map[string][]string{},
	}
	for name, keys := range s.Bindings() {
		for _, key := range keys {
			key = config.Normalize(key)
			if second, ok := strings.CutPrefix(key, "leader "); ok {
				k.chord[second] = name
				k.chordKeys[name] = append(k.chordKeys[name], second)
			} else {
				k.direct[key] = name
			}
		}
	}
	return k
}

// first returns the first direct binding of an action, for hints.
func (k keymap) first(action string) string {
	for _, a := range config.Actions {
		if a.Name != action {
			continue
		}
		for _, key := range a.Defaults {
			if k.direct[config.Normalize(key)] == action {
				return key
			}
		}
	}
	for key, a := range k.direct {
		if a == action {
			return key
		}
	}
	return ""
}

// shortKey abbreviates ctrl+x to ^x for compact hints.
func shortKey(key string) string {
	if rest, ok := strings.CutPrefix(key, "ctrl+"); ok && !strings.Contains(rest, "+") {
		return "^" + rest
	}
	return key
}

// Resizing repeats: after one of these the leader stays active, so
// ctrl+g → → → widens the explorer three steps; any other key ends it.
var repeatable = map[string]bool{
	"grow_explorer": true, "shrink_explorer": true,
	"grow_query": true, "shrink_query": true,
}

// do runs an app-wide action.
func (m *Model) do(action string) tea.Cmd {
	switch action {
	case "quit":
		return tea.Quit
	case "focus_explorer":
		m.setFocus(focusExplorer)
	case "focus_query":
		m.setFocus(focusQuery)
	case "focus_results":
		m.setFocus(focusResults)
	case "full_screen":
		m.setFullScreen(!m.split.full)
	case "full_screen_explorer", "full_screen_query", "full_screen_results":
		m.setFocus(map[string]focus{
			"full_screen_explorer": focusExplorer,
			"full_screen_query":    focusQuery,
			"full_screen_results":  focusResults,
		}[action])
		m.setFullScreen(true)
	case "run":
		if t := m.query.current(); t != nil {
			return m.query.Run(t)
		}
	case "new_tab":
		m.query.NewTab(m.explorer.Context())
		m.setFocus(focusQuery)
		m.saveSession()
	case "open_file":
		t := m.query.current()
		if t == nil {
			t = &tab{}
		}
		return func() tea.Msg { return promptPathMsg{purpose: pathOpen, tab: t} }
	case "grow_explorer", "shrink_explorer", "grow_query", "shrink_query", "reset_layout":
		m.resizeAction(action)
	}
	return nil
}

// leaderHelp lists what can follow the leader key.
func (m Model) leaderHelp(maxWidth int) string {
	var rows []string
	keyW := 0
	type row struct{ keys, desc string }
	var items []row
	for _, a := range config.Actions {
		keys := m.keys.chordKeys[a.Name]
		if len(keys) == 0 {
			continue
		}
		labels := make([]string, len(keys))
		for i, k := range keys {
			labels[i] = keyLabel(k)
		}
		r := row{strings.Join(labels, " "), a.Description}
		keyW = max(keyW, lipgloss.Width(r.keys))
		items = append(items, r)
	}
	for _, r := range items {
		rows = append(rows, accentStyle.Render(r.keys+strings.Repeat(" ", keyW-lipgloss.Width(r.keys)))+"  "+textStyle.Render(r.desc))
	}
	width := min(lipgloss.Width(strings.Join(rows, "\n"))+6, maxWidth)
	body := lipgloss.NewStyle().Padding(1, 2).Render(strings.Join(rows, "\n"))
	p := pane{title: m.keys.leader + " …", footer: hints("esc", "cancel")}
	return p.render(body, width, lipgloss.Height(body)+2, true)
}

// keyLabel shortens key names for the help pop-up.
func keyLabel(k string) string {
	switch k {
	case "left":
		return "←"
	case "right":
		return "→"
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "enter":
		return "⏎"
	}
	return k
}
