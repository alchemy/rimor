package main

import (
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
	"rimor.dev/internal/ui"
)

func main() {
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
