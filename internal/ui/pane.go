package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// pane is a bordered box with its title and an optional footer drawn into
// the border itself:
//
//	╭─ 1 Explorer ─────╮
//	│                  │
//	╰─────── footer ───╯
//
// An index of zero draws no badge. A label, already styled, replaces the
// title (the query pane puts its tabs there); a status sits at the left of
// the bottom edge, the footer at the right.
//
// Hot sides are resize handles under the mouse; they draw heavy lines.
type pane struct {
	index  int
	title  string
	label  string
	status string
	footer string

	hotLeft, hotRight, hotTop, hotBottom bool

	// corner sits at the right end of the top edge, e.g. a mode indicator.
	corner string
}

var handleStyle = lipgloss.NewStyle().Foreground(colorLavender)

// render draws the pane at exactly width×height cells.
func (p pane) render(body string, width, height int, focused bool) string {
	if width < 2 || height < 2 {
		return ""
	}
	innerW, innerH := width-2, height-2

	border, title, badge := borderStyle, titleStyle, badgeStyle
	if focused {
		border, title, badge = borderFocusedStyle, titleFocusedStyle, badgeFocusedStyle
	}

	// edge draws border glyphs, heavy and highlighted on a hot side.
	edge := func(hot bool, s string) string {
		if hot {
			return handleStyle.Render(strings.NewReplacer("─", "━", "│", "┃").Replace(s))
		}
		return border.Render(s)
	}

	var b strings.Builder

	// Top edge with badge and title.
	label := " " + title.Render(p.title) + " "
	if p.label != "" {
		label = " " + p.label + " "
	}
	if p.index > 0 {
		label = badge.Render(fmt.Sprintf(" %d ", p.index)) + label
	}
	corner := ""
	if p.corner != "" && lipgloss.Width(p.corner)+lipgloss.Width(label)+6 <= innerW {
		corner = " " + p.corner + " "
	}
	label = ansi.Truncate(label, max(innerW-2-lipgloss.Width(corner), 0), "…")
	fill := innerW - 1 - lipgloss.Width(label)
	if corner != "" {
		fill -= lipgloss.Width(corner) + 1
		b.WriteString(edge(p.hotTop, "╭─") + label + edge(p.hotTop, strings.Repeat("─", max(fill, 0))) +
			corner + edge(p.hotTop, "─╮"))
	} else {
		b.WriteString(edge(p.hotTop, "╭─") + label + edge(p.hotTop, strings.Repeat("─", max(fill, 0))+"╮"))
	}
	b.WriteByte('\n')

	// Body, clipped and padded to the inner box.
	lines := strings.Split(body, "\n")
	for i := range innerH {
		line := ""
		if i < len(lines) {
			line = ansi.Truncate(lines[i], innerW, "…")
		}
		pad := max(innerW-lipgloss.Width(line), 0)
		b.WriteString(edge(p.hotLeft, "│") + line + strings.Repeat(" ", pad) + edge(p.hotRight, "│"))
		b.WriteByte('\n')
	}

	// Bottom edge: status on the left, footer on the right. The footer goes
	// first when both do not fit.
	status, foot := "", ""
	if p.status != "" {
		status = " " + ansi.Truncate(p.status, max(innerW-4, 0), "…") + " "
	}
	if p.footer != "" && lipgloss.Width(status)+lipgloss.Width(p.footer)+5 <= innerW {
		foot = " " + p.footer + " "
	}
	b.WriteString(edge(p.hotBottom, "╰"))
	fill = innerW - 1 - lipgloss.Width(foot)
	if status != "" {
		b.WriteString(edge(p.hotBottom, "─") + status)
		fill -= 1 + lipgloss.Width(status)
	}
	b.WriteString(edge(p.hotBottom, strings.Repeat("─", max(fill, 0))) + foot + edge(p.hotBottom, "─╯"))

	return b.String()
}
