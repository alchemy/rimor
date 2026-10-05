package ui

import (
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
	"rimor.dev/internal/paths"
)

// nerdFont selects Nerd Font glyphs over plain Unicode; see setIcons.
var nerdFont = true

// setIcons applies the icons setting: "nerd", "plain", or "auto" to guess
// from the terminal and the installed fonts.
func setIcons(mode string) {
	switch mode {
	case config.IconsNerd:
		nerdFont = true
	case config.IconsPlain:
		nerdFont = false
	default:
		nerdFont = detectNerdFont()
	}
}

// detectNerdFont guesses whether Nerd Font glyphs will render. Some
// terminals bundle the symbols; otherwise an installed Nerd Font is a good
// sign the terminal uses one.
func detectNerdFont() bool {
	if os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("TERM") == "xterm-kitty" {
		return true
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "ghostty", "WezTerm":
		return true
	case "Apple_Terminal":
		return false // cannot use the Nerd Font symbols even when installed
	}
	return nerdFontInstalled()
}

// nerdFontInstalled looks for a Nerd Font in the usual font directories.
func nerdFontInstalled() bool {
	home := paths.Home()
	var dirs []string
	switch runtime.GOOS {
	case "windows":
		dirs = []string{
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Windows", "Fonts"),
			filepath.Join(os.Getenv("WINDIR"), "Fonts"),
		}
	case "darwin":
		dirs = []string{filepath.Join(home, "Library", "Fonts"), "/Library/Fonts"}
	default:
		dirs = []string{
			filepath.Join(home, ".local", "share", "fonts"), filepath.Join(home, ".fonts"),
			"/usr/share/fonts", "/usr/local/share/fonts",
		}
	}
	for _, dir := range dirs {
		if dir != "" && hasNerdFont(dir, 3) {
			return true
		}
	}
	return false
}

// hasNerdFont searches dir, depth levels deep, for a file named like a
// Nerd Font ("…NerdFont…" or "… NF …").
func hasNerdFont(dir string, depth int) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if e.IsDir() {
			if depth > 0 && hasNerdFont(filepath.Join(dir, e.Name()), depth-1) {
				return true
			}
			continue
		}
		if strings.Contains(name, "nerdfont") || strings.Contains(name, "nerd font") || strings.Contains(name, "nerd-font") {
			return true
		}
	}
	return false
}

// icon is a glyph and its colour. The colour points at a theme variable,
// so icons follow theme changes.
type icon struct {
	nerd, plain string
	color       *color.Color
}

func (i icon) String() string {
	if nerdFont {
		return i.nerd
	}
	return i.plain
}

// The plain glyphs are all in JetBrains Mono, so in a terminal (or on the
// web page) using it none falls back to another font, whose width may not
// match the cell.
var (
	iconAdd        = icon{"", "+", &colorAccent}
	iconServer     = icon{"\uf233", "◉", &colorBlue}
	iconDatabase   = icon{"\uf1c0", "◈", &colorBlue}
	iconSchema     = icon{"", "◇", &colorLavender}
	iconFolder     = icon{"", "▪", &colorOverlay}
	iconFolderOpen = icon{"", "▫", &colorOverlay}
	iconTable      = icon{"", "◫", &colorBlue}
	iconView       = icon{"", "◎", &colorTeal}
	iconMatView    = icon{"", "⊙", &colorSapphire}
	iconFunction   = icon{"", "ƒ", &colorPeach}
	iconProcedure  = icon{"", "§", &colorPeach}
	iconSequence   = icon{"", "#", &colorPink}
	iconIndex      = icon{"", "≡", &colorLavender}
	iconTrigger    = icon{"", "↝", &colorYellow}
	iconColumn     = icon{"", "·", &colorTitle}
	iconAgent      = icon{"\U000F0674", "✶", &colorAccent}
	iconForeignKey = icon{"\uf0c1", "↗", &colorSapphire}
	iconKey        = icon{"", "◆", &colorYellow}
)

// driverIcon is a server for engines with several databases and a database
// for single-file ones, in the engine's colour.
func driverIcon(d db.Driver) icon {
	i := iconServer
	if !d.HasDatabases() {
		i = iconDatabase
	}
	i.color = driverColor(d)
	return i
}

func driverColor(d db.Driver) *color.Color {
	switch d {
	case db.Postgres:
		return &colorBlue
	case db.SQLite:
		return &colorTeal
	case db.SQLServer:
		return &colorPeach
	}
	return &colorTitle
}

func objectIcon(o db.Object, expanded bool) icon {
	switch o.Kind {
	case db.KindFolder:
		if expanded {
			return iconFolderOpen
		}
		return iconFolder
	case db.KindDatabase:
		i := iconDatabase
		i.color = &colorLavender
		return i
	case db.KindSchema:
		return iconSchema
	case db.KindTable:
		return iconTable
	case db.KindView:
		return iconView
	case db.KindMatView:
		return iconMatView
	case db.KindFunction:
		return iconFunction
	case db.KindProcedure:
		return iconProcedure
	case db.KindSequence:
		return iconSequence
	case db.KindIndex:
		if o.Primary {
			return iconKey
		}
		return iconIndex
	case db.KindTrigger:
		return iconTrigger
	case db.KindForeignKey:
		return iconForeignKey
	case db.KindColumn:
		if o.Primary {
			return iconKey
		}
		return iconColumn
	}
	return iconColumn
}
