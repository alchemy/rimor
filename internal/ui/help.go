package ui

import (
	"strings"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
)

// The help overlay (F1, or ? outside text input) lists the keys that apply
// where the user is, then the app-wide ones; tab shows every key table,
// and typing filters by key or description.

type helpRow struct{ keys, desc string }

type helpGroup struct {
	title string
	rows  []helpRow
}

type helpOverlay struct {
	place   string
	here    []helpGroup
	all     []helpGroup
	showAll bool
	filter  string
	offset  int

	width, height int
}

func newHelp(m *Model) *helpOverlay {
	place, scopes := m.hereScopes()
	h := &helpOverlay{place: place}
	for _, s := range scopes {
		h.here = append(h.here, scopeGroup(s, m, true))
	}
	global := m.globalGroup()
	h.here = append(h.here, global)
	h.all = []helpGroup{global}
	for _, s := range allScopes {
		h.all = append(h.all, scopeGroup(s, m, false))
	}
	h.setSize(m.dialogWidth(), m.dialogHeight())
	return h
}

// scopeGroup turns a key table into rows; onlyNow keeps the bindings that
// apply in the current state.
func scopeGroup(s keyScope, m *Model, onlyNow bool) helpGroup {
	g := helpGroup{title: s.title}
	for _, b := range s.keys {
		if onlyNow && b.when != nil && !b.when(m) {
			continue
		}
		labels := make([]string, len(b.keys))
		for i, k := range b.keys {
			labels[i] = helpKey(k)
		}
		g.rows = append(g.rows, helpRow{strings.Join(labels, " "), b.desc})
	}
	return g
}

// globalGroup lists the app-wide actions with their current bindings: the
// first direct key the terminal can report, then the leader sequences.
func (m *Model) globalGroup() helpGroup {
	g := helpGroup{title: "Everywhere"}
	leader := helpKey(m.keys.leader)
	for _, a := range config.Actions {
		var labels []string
		if k := m.keys.first(a.Name, m.disambiguated); k != "" {
			labels = append(labels, helpKey(k))
		}
		// One leader alternative is enough: the others are the same key on
		// other keyboard layouts.
		if chords := m.keys.chordKeys[a.Name]; len(chords) > 0 {
			labels = append(labels, leader+" "+helpKey(chords[0]))
		}
		if len(labels) == 0 {
			continue
		}
		g.rows = append(g.rows, helpRow{strings.Join(labels, " · "), a.Description})
	}
	return g
}

// helpKey writes a key the way the help shows it: arrows and ⏎ as symbols,
// ctrl+x as ^x.
func helpKey(k string) string {
	switch k {
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "left":
		return "←"
	case "right":
		return "→"
	case "enter":
		return "⏎"
	case "pgup":
		return "PgUp"
	case "pgdown":
		return "PgDn"
	}
	k = strings.ReplaceAll(k, "enter", "⏎")
	return keyHint(k)
}

func (h *helpOverlay) setSize(maxWidth, maxHeight int) {
	h.width, h.height = min(80, maxWidth), maxHeight
}

// lines renders the visible groups, filtered.
func (h *helpOverlay) lines() []string {
	groups := h.here
	if h.showAll {
		groups = h.all
	}
	filter := strings.ToLower(h.filter)
	keyW := 0
	for _, g := range groups {
		for _, r := range g.rows {
			keyW = max(keyW, lipgloss.Width(r.keys))
		}
	}
	keyW = min(keyW, 18) // longer key lists take a line of their own
	inner := h.width - 6

	var out []string
	for _, g := range groups {
		var rows []string
		for _, r := range g.rows {
			if filter != "" && !strings.Contains(strings.ToLower(r.keys+" "+r.desc+" "+g.title), filter) {
				continue
			}
			keys := r.keys
			if lipgloss.Width(keys) > keyW {
				// Long key lists wrap onto their own line.
				rows = append(rows, accentStyle.Render(keys))
				keys = ""
			}
			pad := strings.Repeat(" ", keyW-lipgloss.Width(keys))
			for i, l := range wrapLines(r.desc, inner-keyW-2, 3) {
				lead := accentStyle.Render(keys) + pad
				if i > 0 {
					lead = strings.Repeat(" ", keyW)
				}
				rows = append(rows, lead+"  "+textStyle.Render(l))
			}
		}
		if len(rows) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, titleStyle.Bold(true).Render(g.title))
		out = append(out, rows...)
	}
	if len(out) == 0 {
		out = []string{mutedStyle.Italic(true).Render("No key matches “" + h.filter + "”.")}
	}
	return out
}

// helpChrome is the rows around the list: borders, filter, spacing.
const helpChrome = 6

func (h *helpOverlay) visible() int { return max(h.height-helpChrome, 3) }

func (h *helpOverlay) scroll(delta int) {
	h.offset = min(max(h.offset+delta, 0), max(len(h.lines())-h.visible(), 0))
}

// Update handles a key; done reports that the overlay should close.
func (h *helpOverlay) Update(msg tea.KeyPressMsg) (done bool) {
	switch msg.String() {
	case "esc":
		if h.filter != "" {
			h.filter, h.offset = "", 0
			return false
		}
		return true
	case "tab":
		h.showAll, h.offset = !h.showAll, 0
	case "up":
		h.scroll(-1)
	case "down":
		h.scroll(1)
	case "pgup":
		h.scroll(-h.visible())
	case "pgdown":
		h.scroll(h.visible())
	case "backspace":
		if r := []rune(h.filter); len(r) > 0 {
			h.filter, h.offset = string(r[:len(r)-1]), 0
		}
	default:
		if msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			h.filter, h.offset = h.filter+msg.Text, 0
		}
	}
	return false
}

func (h *helpOverlay) View() string {
	lines := h.lines()
	h.offset = min(h.offset, max(len(lines)-h.visible(), 0))
	end := min(h.offset+h.visible(), len(lines))

	filter := mutedStyle.Italic(true).Render("type to filter")
	if h.filter != "" {
		filter = textStyle.Render(h.filter)
	}
	rows := []string{"", accentStyle.Render("› ") + filter, ""}
	rows = append(rows, lines[h.offset:end]...)
	if end < len(lines) {
		rows = append(rows, mutedStyle.Render("↓ more"))
	}
	body := lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(rows, "\n"))

	title := "Keys · " + h.place
	other := "all keys"
	if h.showAll {
		title, other = "Keys · all", "keys here"
	}
	p := pane{title: title, footer: hints("tab", other, "esc", "close")}
	return p.render(body, h.width, lipgloss.Height(body)+3, true)
}
