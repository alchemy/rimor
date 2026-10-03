package ui

import (
	"database/sql"
	"fmt"
	"html"
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// TestSiteScreens renders the screens shown on rimor.dev and writes them
// into the page, between <!-- screen:NAME --> and <!-- /screen:NAME -->:
//
//	RIMOR_SITE=site go test ./internal/ui -run TestSiteScreens
//
// The screens are the real UI, drawn by the same code as in a terminal,
// with the escape codes turned into HTML.
func TestSiteScreens(t *testing.T) {
	site := os.Getenv("RIMOR_SITE")
	if site == "" {
		t.Skip("RIMOR_SITE not set")
	}
	if !filepath.IsAbs(site) {
		site = filepath.Join("..", "..", site) // the test runs in internal/ui
	}
	t.Setenv("NO_COLOR", "")
	defer applyPalette(darkPalette)

	page := filepath.Join(site, "index.html")
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, theme := range []string{config.ThemeDark, config.ThemeLight, config.ThemeTerminal} {
		screen := siteScreen(t, theme)
		re := regexp.MustCompile(`(?s)(<!-- screen:` + theme + ` -->).*?(<!-- /screen:` + theme + ` -->)`)
		if !re.MatchString(out) {
			t.Fatalf("index.html has no markers for %s", theme)
		}
		out = re.ReplaceAllLiteralString(out, "<!-- screen:"+theme+" -->"+screen+"<!-- /screen:"+theme+" -->")
	}
	if err := os.WriteFile(page, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gruvbox is the terminal palette the "terminal" screen is shown with:
// warm, and unlike the dark theme's cool Catppuccin.
var gruvbox = struct {
	fg, bg color.Color
	ansi   [16]color.Color
}{
	fg: lipgloss.Color("#ebdbb2"), bg: lipgloss.Color("#282828"),
	ansi: [16]color.Color{
		lipgloss.Color("#282828"), lipgloss.Color("#cc241d"), lipgloss.Color("#98971a"), lipgloss.Color("#d79921"),
		lipgloss.Color("#458588"), lipgloss.Color("#b16286"), lipgloss.Color("#689d6a"), lipgloss.Color("#a89984"),
		lipgloss.Color("#928374"), lipgloss.Color("#fb4934"), lipgloss.Color("#b8bb26"), lipgloss.Color("#fabd2f"),
		lipgloss.Color("#83a598"), lipgloss.Color("#d3869b"), lipgloss.Color("#8ec07c"), lipgloss.Color("#ebdbb2"),
	},
}

// siteScreen drives rimor into a showcase state and returns it as HTML.
func siteScreen(t *testing.T, theme string) string {
	dir := t.TempDir()
	path := filepath.Join(dir, "shop.db")
	c, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT NOT NULL, city TEXT, since DATE)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER REFERENCES customers, total REAL, placed TEXT)`,
		`CREATE INDEX orders_customer ON orders(customer_id)`,
		`CREATE VIEW big_orders AS SELECT * FROM orders WHERE total > 100`,
		`INSERT INTO customers VALUES
			(1, 'Ada Lovelace', 'London', '2024-03-01'), (2, 'Grace Hopper', 'Arlington', '2024-05-17'),
			(3, 'Edsger Dijkstra', NULL, '2025-01-09'), (4, 'Barbara Liskov', 'Boston', '2025-06-30'),
			(5, 'Alan Turing', 'Wilmslow', '2025-08-12'), (6, 'Frances Allen', 'Peru', '2026-02-03')`,
		`INSERT INTO orders VALUES (1,1,120.5,'2026-09-01'), (2,1,35,'2026-09-03'), (3,2,980,'2026-09-12'),
			(4,4,42.25,'2026-09-20'), (5,2,15,'2026-09-28'), (6,5,310,'2026-09-29'), (7,6,77.7,'2026-09-30')`,
	} {
		if _, err := c.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()

	store := db.Store{Path: filepath.Join(dir, "c.json")}
	store.Save([]db.Config{{Name: "shop", Driver: db.SQLite, DSN: path}})
	settings := config.Defaults()
	settings.Theme, settings.Icons = theme, config.IconsPlain // the web font has no Nerd Font glyphs
	d := &driver{t: t, m: New(store, "", settings)}
	d.send(tea.WindowSizeMsg{Width: 112, Height: 28})
	if theme == config.ThemeTerminal {
		d.send(tea.BackgroundColorMsg{Color: gruvbox.bg})
	}
	d.send(tea.KeyboardEnhancementsMsg{Flags: 1}) // show ^⏎ as a modern terminal would

	// Explorer: shop → Tables → customers → Columns.
	d.key("down", "enter", "down", "enter", "down", "enter", "down", "enter")

	// Two saved tabs in the shop context; the starting untitled one goes.
	saveAs := func(name string) {
		d.key("ctrl+s")
		for range 300 {
			d.key("backspace")
		}
		d.typeText(filepath.Join(dir, name))
		d.key("enter")
	}
	d.key("ctrl+t")
	d.send(tea.PasteMsg{Content: "SELECT * FROM orders WHERE total > 100;"})
	saveAs("orders.sql")
	d.key("alt+[", "ctrl+w") // close untitled-1

	d.key("ctrl+t")
	d.send(tea.PasteMsg{Content: `-- Best customers this month
SELECT c.name, c.city,
       count(o.id)  AS orders,
       sum(o.total) AS spent
FROM customers c
LEFT JOIN orders o ON o.customer_id = c.id
GROUP BY c.id
ORDER BY spent DESC NULLS LAST`})
	saveAs("best-customers.sql")
	d.key("ctrl+home")
	for range 3 { // room for the whole query
		d.key("alt+shift+down")
	}
	d.key("ctrl+enter")
	d.key("alt+3", "down", "right")
	d.screen() // checks the width of every row
	return toHTML(d.m.render(), theme)
}

var sgr = regexp.MustCompile("\x1b\\[([0-9;]*)m")

// defaults are each screen's default foreground and background, as the
// page's CSS sets them.
var defaults = map[string][2]color.Color{
	config.ThemeDark:     {lipgloss.Color("#cdd6f4"), lipgloss.Color("#1e1e2e")},
	config.ThemeLight:    {lipgloss.Color("#4c4f69"), lipgloss.Color("#eff1f5")},
	config.ThemeTerminal: {gruvbox.fg, gruvbox.bg},
}

// toHTML turns SGR-coloured text into spans with inline colours. Basic
// ANSI colours resolve through Gruvbox for the terminal theme.
func toHTML(s, theme string) string {
	type state struct {
		fg, bg                              color.Color
		bold, faint, italic, under, reverse bool
	}
	var st state
	basic := func(n int) color.Color {
		if theme == config.ThemeTerminal {
			return gruvbox.ansi[n]
		}
		return nil
	}
	hex := func(c color.Color) string {
		r, g, b, _ := c.RGBA()
		return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	}

	var b strings.Builder
	open := false
	flush := func() {
		if open {
			b.WriteString("</span>")
			open = false
		}
		fg, bg := st.fg, st.bg
		if st.faint {
			// Terminals draw faint text about halfway to the background.
			if fg == nil {
				fg = defaults[theme][0]
			}
			fg = mix(fg, defaults[theme][1], 0.45)
		}
		if st.reverse {
			// Reverse video swaps in the screen's default colours.
			if fg == nil {
				fg = defaults[theme][0]
			}
			if bg == nil {
				bg = defaults[theme][1]
			}
			fg, bg = bg, fg
		}
		var css []string
		if fg != nil {
			css = append(css, "color:"+hex(fg))
		}
		if bg != nil {
			css = append(css, "background:"+hex(bg))
		}
		if st.bold {
			css = append(css, "font-weight:700")
		}
		if st.italic {
			css = append(css, "font-style:italic")
		}
		if st.under {
			css = append(css, "text-decoration:underline")
		}
		if len(css) > 0 {
			b.WriteString(`<span style="` + strings.Join(css, ";") + `">`)
			open = true
		}
	}

	last := 0
	for _, m := range sgr.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(html.EscapeString(s[last:m[0]]))
		last = m[1]
		params := strings.Split(s[m[2]:m[3]], ";")
		for i := 0; i < len(params); i++ {
			n, _ := strconv.Atoi(params[i])
			switch {
			case n == 0:
				st = state{}
			case n == 1:
				st.bold = true
			case n == 2:
				st.faint = true
			case n == 3:
				st.italic = true
			case n == 4:
				st.under = true
			case n == 7:
				st.reverse = true
			case n == 22:
				st.bold, st.faint = false, false
			case n == 23:
				st.italic = false
			case n == 24:
				st.under = false
			case n == 27:
				st.reverse = false
			case n >= 30 && n <= 37:
				st.fg = basic(n - 30)
			case n >= 90 && n <= 97:
				st.fg = basic(n - 90 + 8)
			case n >= 40 && n <= 47:
				st.bg = basic(n - 40)
			case n >= 100 && n <= 107:
				st.bg = basic(n - 100 + 8)
			case n == 39:
				st.fg = nil
			case n == 49:
				st.bg = nil
			case (n == 38 || n == 48) && i+1 < len(params):
				var c color.Color
				switch params[i+1] {
				case "2":
					if i+4 < len(params) {
						r, _ := strconv.Atoi(params[i+2])
						g, _ := strconv.Atoi(params[i+3])
						bl, _ := strconv.Atoi(params[i+4])
						c = color.RGBA{uint8(r), uint8(g), uint8(bl), 0xff}
						i += 4
					}
				case "5":
					if i+2 < len(params) {
						idx, _ := strconv.Atoi(params[i+2])
						if idx < 16 {
							c = basic(idx)
						}
						i += 2
					}
				}
				if n == 38 {
					st.fg = c
				} else {
					st.bg = c
				}
			}
		}
		flush()
	}
	b.WriteString(html.EscapeString(s[last:]))
	if open {
		b.WriteString("</span>")
	}
	return b.String()
}
