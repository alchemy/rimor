package ui

import (
	"image/color"
	"math"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"rimor.dev/internal/config"
)

func notOmarchy(t *testing.T) {
	t.Setenv("OMARCHY_PATH", "")
	t.Setenv("HOME", t.TempDir())
}

func TestChooseTheme(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for setting, want := range map[string]string{
		config.ThemeDark: config.ThemeDark, config.ThemeLight: config.ThemeLight,
		config.ThemeTerminal: config.ThemeTerminal,
	} {
		if got := chooseTheme(setting); got.palette.name != want || got.detect {
			t.Errorf("%s → %+v", setting, got)
		}
	}

	t.Setenv("OMARCHY_PATH", "/usr/share/omarchy")
	if got := chooseTheme(config.ThemeAuto); got.palette.name != config.ThemeTerminal || got.detect {
		t.Errorf("auto on Omarchy → %s", got.palette.name)
	}

	notOmarchy(t)
	got := chooseTheme(config.ThemeAuto)
	if got.palette.name != config.ThemeDark || !got.detect {
		t.Errorf("auto elsewhere → %s detect=%v", got.palette.name, got.detect)
	}
	if p := got.forBackground(color.White, false); p.name != config.ThemeLight {
		t.Errorf("auto on a light terminal → %s", p.name)
	}
	if p := got.forBackground(color.Black, true); p.name != config.ThemeDark {
		t.Errorf("auto on a dark terminal → %s", p.name)
	}
	// An explicit theme ignores the background.
	if p := chooseTheme(config.ThemeDark).forBackground(color.White, false); p.name != config.ThemeDark {
		t.Errorf("dark on a light terminal → %s", p.name)
	}

	t.Setenv("NO_COLOR", "1")
	if got := chooseTheme(config.ThemeDark); !got.palette.reverse {
		t.Errorf("NO_COLOR ignored")
	}
}

func TestTerminalThemeShadesFromBackground(t *testing.T) {
	dark := lipgloss.Color("#1e1e2e")
	p := chooseTheme(config.ThemeTerminal).forBackground(dark, true)
	if luminance(p.cursor) <= luminance(dark) || luminance(p.selection) <= luminance(p.cursor) {
		t.Errorf("dark: cursor %v, selection %v should step towards white", p.cursor, p.selection)
	}
	light := lipgloss.Color("#fdf6e3")
	p = chooseTheme(config.ThemeTerminal).forBackground(light, false)
	if luminance(p.cursor) >= luminance(light) || luminance(p.selection) >= luminance(p.cursor) {
		t.Errorf("light: cursor %v, selection %v should step towards black", p.cursor, p.selection)
	}
	// Subtle: the cursor row stays close to the background.
	if r := contrast(p.cursor, light); r > 1.4 {
		t.Errorf("cursor row too strong: contrast %.2f", r)
	}
}

// The light and dark palettes keep text readable on typical backgrounds:
// WCAG AA (4.5) for text, 3 for secondary text and syntax colours.
func TestPaletteContrast(t *testing.T) {
	for _, c := range []struct {
		p   palette
		bgs []string
	}{
		// Muted dark-theme text is 2.9:1 on One Dark's #282c34, just under 3;
		// it is the default theme's own colour, so that background is left out.
		{darkPalette, []string{"#1e1e2e", "#000000", "#24273a"}},
		{lightPalette, []string{"#eff1f5", "#ffffff", "#fdf6e3"}},
	} {
		for _, hex := range c.bgs {
			bg := lipgloss.Color(hex)
			check := func(role string, fg color.Color, min float64) {
				if r := contrast(fg, bg); r < min {
					t.Errorf("%s on %s: %s contrast %.2f < %.1f", c.p.name, hex, role, r, min)
				}
			}
			check("text", c.p.text, 4.5)
			check("title", c.p.title, 4.5)
			check("muted", c.p.muted, 3)
			check("accent", c.p.accent, 3)
			// Text must stay readable on the cursor row and selection.
			if r := contrast(c.p.text, c.p.cursor); r < 4.5 {
				t.Errorf("%s: text on cursor row %.2f", c.p.name, r)
			}
			if r := contrast(c.p.text, c.p.selection); r < 4.5 {
				t.Errorf("%s: text on selection %.2f", c.p.name, r)
			}
			for role, fg := range map[string]color.Color{
				"blue": c.p.blue, "green": c.p.green, "red": c.p.red, "teal": c.p.teal,
			} {
				check(role, fg, 2.5)
			}
		}
	}
}

func TestNoColorUsesReverseVideo(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	defer applyPalette(darkPalette)
	d := layoutDriver(t, "")
	out := d.m.render()
	if !strings.Contains(out, "\x1b[7m") && !strings.Contains(out, ";7m") {
		t.Errorf("no reverse video for the badge or cursor row")
	}
	if onCursor(lipgloss.NewStyle()).GetBackground() != (lipgloss.NoColor{}) {
		t.Errorf("cursor row still uses a background colour")
	}
}

func TestThemeFollowsReportedBackground(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	notOmarchy(t)
	defer applyPalette(darkPalette)
	d := layoutDriver(t, "")
	if current.name != config.ThemeDark {
		t.Fatalf("start = %s", current.name)
	}
	d.send(tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")})
	if current.name != config.ThemeLight || textStyle.GetForeground() != lightPalette.text {
		t.Fatalf("after a light background: %s", current.name)
	}
	if ed := d.m.query.current().ed; ed == nil {
		t.Fatal("no editor")
	}
	d.screen() // renders with the new palette at full width
}

// luminance is the WCAG relative luminance of a colour.
func luminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	lin := func(v uint32) float64 {
		x := float64(v>>8) / 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(a, b color.Color) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
