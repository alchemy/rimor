package ui

import "charm.land/lipgloss/v2"

// Palette inspired by Catppuccin Mocha: soft, low-contrast chrome so the
// data stays the brightest thing on screen.
var (
	colorAccent = lipgloss.Color("#cba6f7") // mauve — focused pane
	colorBorder = lipgloss.Color("#45475a") // surface1 — resting chrome
	colorText   = lipgloss.Color("#cdd6f4") // text
	colorTitle  = lipgloss.Color("#a6adc8") // subtext0
	colorMuted  = lipgloss.Color("#6c7086") // overlay0
	colorBadge  = lipgloss.Color("#1e1e2e") // base — text on accent
	colorCursor = lipgloss.Color("#313244") // surface0 — selected row

	colorBlue     = lipgloss.Color("#89b4fa")
	colorSapphire = lipgloss.Color("#74c7ec")
	colorTeal     = lipgloss.Color("#94e2d5")
	colorGreen    = lipgloss.Color("#a6e3a1")
	colorYellow   = lipgloss.Color("#f9e2af")
	colorPeach    = lipgloss.Color("#fab387")
	colorRed      = lipgloss.Color("#f38ba8")
	colorPink     = lipgloss.Color("#f5c2e7")
	colorLavender = lipgloss.Color("#b4befe")
	colorOverlay  = lipgloss.Color("#9399b2") // overlay2
)

var (
	borderStyle        = lipgloss.NewStyle().Foreground(colorBorder)
	borderFocusedStyle = lipgloss.NewStyle().Foreground(colorAccent)

	titleStyle        = lipgloss.NewStyle().Foreground(colorTitle)
	titleFocusedStyle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)

	badgeStyle        = lipgloss.NewStyle().Foreground(colorMuted)
	badgeFocusedStyle = lipgloss.NewStyle().Foreground(colorBadge).Background(colorAccent).Bold(true)

	textStyle   = lipgloss.NewStyle().Foreground(colorText)
	mutedStyle  = lipgloss.NewStyle().Foreground(colorMuted)
	hintKey     = lipgloss.NewStyle().Foreground(colorTitle)
	accentStyle = lipgloss.NewStyle().Foreground(colorAccent)
	errorStyle  = lipgloss.NewStyle().Foreground(colorRed)
	okStyle     = lipgloss.NewStyle().Foreground(colorGreen)
)

// hints renders "key action · key action" pairs for borders and footers.
func hints(pairs ...string) string {
	var s string
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			s += mutedStyle.Render(" · ")
		}
		s += hintKey.Render(pairs[i]) + mutedStyle.Render(" "+pairs[i+1])
	}
	return s
}
