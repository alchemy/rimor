package ui

import (
	"image/color"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"rimor.dev/internal/config"
	"rimor.dev/internal/editor"
	"rimor.dev/internal/paths"
)

// palette is a colour theme. Roles are named after the dark theme's
// Catppuccin colours they were first drawn with.
type palette struct {
	name string

	accent    color.Color // focused pane, keywords, the active tab
	border    color.Color // resting chrome
	text      color.Color // data and names
	title     color.Color // secondary text: pane titles, hint keys
	muted     color.Color // tertiary text: hints, NULL
	onAccent  color.Color // text on an accent background (the focused badge)
	cursor    color.Color // background of the selected row
	selection color.Color // background of selected text and the grid cell

	blue, sapphire, teal, green, yellow, peach, red, pink, lavender color.Color
	overlay, sky, maroon, rosewater, lineNumber                     color.Color

	// reverse marks the cursor row, selection and badge with reverse video
	// instead of background colours, for NO_COLOR.
	reverse bool
	// faint draws muted text (and the greys built on it) in the default
	// colour with the faint attribute, which terminals render at reduced
	// intensity from their own text colour: readable in any palette, where
	// a fixed grey such as ANSI 8 can vanish into the background.
	faint bool
}

// Catppuccin Mocha: soft, low-contrast chrome so the data stays the
// brightest thing on screen.
var darkPalette = palette{
	name:   config.ThemeDark,
	accent: lipgloss.Color("#cba6f7"), border: lipgloss.Color("#45475a"),
	text: lipgloss.Color("#cdd6f4"), title: lipgloss.Color("#a6adc8"), muted: lipgloss.Color("#6c7086"),
	onAccent: lipgloss.Color("#1e1e2e"), cursor: lipgloss.Color("#313244"), selection: lipgloss.Color("#45475a"),
	blue: lipgloss.Color("#89b4fa"), sapphire: lipgloss.Color("#74c7ec"), teal: lipgloss.Color("#94e2d5"),
	green: lipgloss.Color("#a6e3a1"), yellow: lipgloss.Color("#f9e2af"), peach: lipgloss.Color("#fab387"),
	red: lipgloss.Color("#f38ba8"), pink: lipgloss.Color("#f5c2e7"), lavender: lipgloss.Color("#b4befe"),
	overlay: lipgloss.Color("#9399b2"), sky: lipgloss.Color("#89dceb"), maroon: lipgloss.Color("#eba0ac"),
	rosewater: lipgloss.Color("#f5e0dc"), lineNumber: lipgloss.Color("#585b70"),
}

// Catppuccin Latte, the same roles on a light background. Text and muted
// are a step darker than Latte's defaults to stay readable on pure white.
var lightPalette = palette{
	name:   config.ThemeLight,
	accent: lipgloss.Color("#8839ef"), border: lipgloss.Color("#bcc0cc"),
	text: lipgloss.Color("#4c4f69"), title: lipgloss.Color("#5c5f77"), muted: lipgloss.Color("#7c7f93"),
	onAccent: lipgloss.Color("#eff1f5"), cursor: lipgloss.Color("#dce0e8"), selection: lipgloss.Color("#ccd0da"),
	blue: lipgloss.Color("#1e66f5"), sapphire: lipgloss.Color("#209fb5"), teal: lipgloss.Color("#179299"),
	green: lipgloss.Color("#40a02b"), yellow: lipgloss.Color("#df8e1d"), peach: lipgloss.Color("#fe640b"),
	red: lipgloss.Color("#d20f39"), pink: lipgloss.Color("#ea76cb"), lavender: lipgloss.Color("#7287fd"),
	overlay: lipgloss.Color("#8c8fa1"), sky: lipgloss.Color("#04a5e5"), maroon: lipgloss.Color("#e64553"),
	rosewater: lipgloss.Color("#dc8a78"), lineNumber: lipgloss.Color("#9ca0b0"),
}

// The terminal's own 16 colours, so rimor follows its theme (on Omarchy,
// the system theme). Text uses the terminal's default foreground, and
// muted text that foreground drawn faint: ANSI bright black is too dim in
// many palettes (Tokyo Night's is barely above its background), so it is
// kept for borders only. The cursor and selection backgrounds are blended
// from the terminal's background once it reports it (see
// terminalBackground); ANSI bright black stands in until then.
var terminalPalette = palette{
	name:   config.ThemeTerminal,
	accent: ansiColor(5), border: ansiColor(8),
	text: lipgloss.NoColor{}, title: lipgloss.NoColor{}, muted: lipgloss.NoColor{},
	onAccent: ansiColor(0), cursor: ansiColor(8), selection: ansiColor(8),
	blue: ansiColor(4), sapphire: ansiColor(6), teal: ansiColor(6), green: ansiColor(2), yellow: ansiColor(3), peach: ansiColor(11),
	red: ansiColor(1), pink: ansiColor(13), lavender: ansiColor(12), overlay: lipgloss.NoColor{}, sky: ansiColor(14), maroon: ansiColor(9),
	rosewater: lipgloss.NoColor{}, lineNumber: lipgloss.NoColor{},
	faint: true,
}

// NO_COLOR (https://no-color.org): no colours at all; selection shows as
// reverse video.
var noColorPalette = palette{
	name: "none", accent: lipgloss.NoColor{}, border: lipgloss.NoColor{}, text: lipgloss.NoColor{},
	title: lipgloss.NoColor{}, muted: lipgloss.NoColor{}, onAccent: lipgloss.NoColor{},
	cursor: lipgloss.NoColor{}, selection: lipgloss.NoColor{},
	blue: lipgloss.NoColor{}, sapphire: lipgloss.NoColor{}, teal: lipgloss.NoColor{}, green: lipgloss.NoColor{},
	yellow: lipgloss.NoColor{}, peach: lipgloss.NoColor{}, red: lipgloss.NoColor{}, pink: lipgloss.NoColor{},
	lavender: lipgloss.NoColor{}, overlay: lipgloss.NoColor{}, sky: lipgloss.NoColor{},
	maroon: lipgloss.NoColor{}, rosewater: lipgloss.NoColor{}, lineNumber: lipgloss.NoColor{},
	reverse: true,
}

// ansiColor is one of the terminal's 16 palette colours, written with the
// classic SGR codes (30–37, 90–97) that every terminal maps to its theme.
func ansiColor(n int) color.Color { return ansi.BasicColor(n) }

// The active theme's colours and styles. applyPalette sets them; icons
// point at the colour variables, so they follow a change too.
var (
	current palette

	colorAccent, colorBorder, colorText, colorTitle, colorMuted  color.Color
	colorBadge, colorCursor, colorSelection                      color.Color
	colorBlue, colorSapphire, colorTeal, colorGreen, colorYellow color.Color
	colorPeach, colorRed, colorPink, colorLavender, colorOverlay color.Color

	borderStyle, borderFocusedStyle  lipgloss.Style
	titleStyle, titleFocusedStyle    lipgloss.Style
	badgeStyle, badgeFocusedStyle    lipgloss.Style
	textStyle, mutedStyle, hintKey   lipgloss.Style
	accentStyle, errorStyle, okStyle lipgloss.Style
	handleStyle                      lipgloss.Style

	editorTheme editor.Theme
)

func init() { applyPalette(darkPalette) }

func applyPalette(p palette) {
	current = p
	colorAccent, colorBorder, colorText, colorTitle, colorMuted = p.accent, p.border, p.text, p.title, p.muted
	colorBadge, colorCursor, colorSelection = p.onAccent, p.cursor, p.selection
	colorBlue, colorSapphire, colorTeal, colorGreen, colorYellow = p.blue, p.sapphire, p.teal, p.green, p.yellow
	colorPeach, colorRed, colorPink, colorLavender, colorOverlay = p.peach, p.red, p.pink, p.lavender, p.overlay

	fg := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	borderStyle, borderFocusedStyle = fg(p.border), fg(p.accent)
	titleStyle, titleFocusedStyle = fg(p.title), fg(p.accent).Bold(true)
	badgeStyle = fg(p.muted).Faint(p.faint)
	badgeFocusedStyle = fg(p.onAccent).Background(p.accent).Bold(true)
	if p.reverse {
		badgeFocusedStyle = lipgloss.NewStyle().Reverse(true).Bold(true)
	}
	textStyle, mutedStyle, hintKey = fg(p.text), fg(p.muted).Faint(p.faint), fg(p.title)
	accentStyle, errorStyle, okStyle = fg(p.accent), fg(p.red), fg(p.green)
	handleStyle = fg(p.lavender)
	if p.reverse {
		accentStyle, handleStyle = accentStyle.Bold(true), handleStyle.Bold(true) // focus without colour
	}

	editorTheme = editor.Theme{
		Text: p.text, Keyword: p.accent, Type: p.yellow, Function: p.blue, String: p.green,
		Number: p.peach, Comment: p.muted, Operator: p.sky, Punctuation: p.overlay,
		Variable: p.maroon, Quoted: p.rosewater, LineNumber: p.lineNumber,
		CurrentLineNumber: p.accent, Selection: p.selection, Placeholder: p.muted,
		ReverseSelection: p.reverse, FaintMuted: p.faint,
	}
}

// iconStyle colours an icon; grey icons are faint where muted text is.
func iconStyle(ic icon) lipgloss.Style {
	s := lipgloss.NewStyle().Foreground(*ic.color)
	if current.faint && (ic.color == &colorMuted || ic.color == &colorOverlay) {
		s = s.Faint(true)
	}
	return s
}

// onCursor marks a style as part of the selected row.
func onCursor(s lipgloss.Style) lipgloss.Style {
	if current.reverse {
		return s.Reverse(true)
	}
	return s.Background(colorCursor)
}

// onSelection marks a style as the selected cell, inside the selected row.
func onSelection(s lipgloss.Style) lipgloss.Style {
	if current.reverse {
		return s.Reverse(true).Underline(true)
	}
	return s.Background(colorSelection)
}

// themeChoice is how the theme setting resolves.
type themeChoice struct {
	palette palette
	// detect: pick dark or light once the terminal reports its background.
	detect bool
}

// chooseTheme resolves the theme setting. NO_COLOR wins over everything;
// "auto" means the terminal's colours on Omarchy, whose system themes
// recolour the terminal, and dark or light from the background elsewhere.
func chooseTheme(setting string) themeChoice {
	if os.Getenv("NO_COLOR") != "" {
		return themeChoice{palette: noColorPalette}
	}
	switch setting {
	case config.ThemeDark:
		return themeChoice{palette: darkPalette}
	case config.ThemeLight:
		return themeChoice{palette: lightPalette}
	case config.ThemeTerminal:
		return themeChoice{palette: terminalPalette}
	}
	if paths.Omarchy() {
		return themeChoice{palette: terminalPalette}
	}
	return themeChoice{palette: darkPalette, detect: true}
}

// forBackground adapts a choice to the terminal's reported background.
func (c themeChoice) forBackground(bg color.Color, dark bool) palette {
	switch {
	case c.detect && dark:
		return darkPalette
	case c.detect:
		return lightPalette
	case c.palette.name == config.ThemeTerminal:
		return terminalBackground(c.palette, bg, dark)
	}
	return c.palette
}

// terminalBackground derives subtle cursor and selection backgrounds from
// the terminal's own background: a step towards white on dark terminals,
// towards black on light ones.
func terminalBackground(p palette, bg color.Color, dark bool) palette {
	towards := color.Color(color.White)
	if !dark {
		towards = color.Black
	}
	p.cursor = mix(bg, towards, 0.10)
	p.selection = mix(bg, towards, 0.22)
	return p
}

// mix blends a towards b by t (0..1) in sRGB.
func mix(a, b color.Color, t float64) color.Color {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	ch := func(x, y uint32) uint8 {
		return uint8((float64(x>>8)*(1-t) + float64(y>>8)*t) + 0.5)
	}
	return color.RGBA{R: ch(ar, br), G: ch(ag, bg), B: ch(ab, bb), A: 0xff}
}

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
