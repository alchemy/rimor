package db

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"rimor.dev/internal/paths"
)

// Store persists connections as JSON. DSNs may carry passwords, so the file
// is readable by the owner only.
type Store struct {
	Path string
}

// DefaultStore returns the store in the config directory, e.g.
// ~/.config/rimor/connections.json (see package paths).
func DefaultStore() (Store, error) {
	dir, err := paths.Config()
	if err != nil {
		return Store{}, err
	}
	return Store{Path: filepath.Join(dir, "connections.json")}, nil
}

// Init creates the store's directory and an empty connections file when
// they do not exist yet, as on first run. Existing files are left alone.
func (s Store) Init() error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(s.Path); errors.Is(err, fs.ErrNotExist) {
		return s.Save(nil)
	} else if err != nil {
		return err
	}
	return nil
}

// Load returns the saved connections; a missing file means none.
func (s Store) Load() ([]Config, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Connections []Config `json:"connections"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	return file.Connections, nil
}

// Save replaces the saved connections atomically.
func (s Store) Save(conns []Config) error {
	if conns == nil {
		conns = []Config{}
	}
	data, err := json.MarshalIndent(struct {
		Connections []Config `json:"connections"`
	}{conns}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}
