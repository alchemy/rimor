package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
	"rimor.dev/internal/ui"
)

// version is set by release builds (-ldflags "-X main.version=v1.2.3").
var version string

// Version is the release version: set at build time, else the module
// version go install records, else "dev" for a local build.
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "rimor %s, a terminal workbench for SQL databases\n\nUsage: rimor [-version]\n", Version())
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("rimor", Version())
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rimor:", err)
		os.Exit(1)
	}
}

func run() error {
	store, err := db.DefaultStore()
	if err != nil {
		return err
	}
	if err := store.Init(); err != nil {
		return fmt.Errorf("setting up %s: %w", filepath.Dir(store.Path), err)
	}
	settings, err := config.Load(filepath.Join(filepath.Dir(store.Path), "config.toml"))
	if err != nil {
		return err
	}
	sessionPath, err := ui.DefaultSessionPath()
	if err != nil {
		return err
	}
	final, err := tea.NewProgram(ui.New(store, sessionPath, settings)).Run()
	if m, ok := final.(ui.Model); ok {
		m.Close()
	}
	return err
}
