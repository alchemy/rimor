package db

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"rimor.dev/internal/paths"
)

// Store persists connections as JSON, readable by the owner only. With
// Secrets set, passwords go there instead of the file (see secrets.go);
// without, connection strings are saved as they are.
type Store struct {
	Path    string
	Secrets Secrets
}

// DefaultStore returns the store in the config directory, e.g.
// ~/.config/rimor/connections.json (see package paths), keeping passwords
// in the system keychain.
func DefaultStore() (Store, error) {
	dir, err := paths.Config()
	if err != nil {
		return Store{}, err
	}
	return Store{Path: filepath.Join(dir, "connections.json"), Secrets: Keychain{Service: "rimor " + dir}}, nil
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

// Load returns the saved connections, with their passwords; a missing
// file means none.
func (s Store) Load() ([]Config, error) {
	conns, err := s.read()
	for i := range conns {
		conns[i] = withSecret(conns[i], s.Secrets)
	}
	return conns, err
}

// read returns the connections as the file has them.
func (s Store) read() ([]Config, error) {
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

// Save replaces the saved connections atomically. Passwords go to the
// keychain, and entries of connections that are gone (deleted, renamed, or
// without a password now) are removed from it. When the keychain refuses
// a password the file keeps it, and Save returns a *KeychainError after
// saving.
func (s Store) Save(conns []Config) error {
	before, _ := s.read()
	out := make([]Config, len(conns))
	var keychainErr error
	for i, c := range conns {
		c.SecretErr = nil
		clean, password, has := splitPassword(c.Driver, c.DSN)
		switch {
		case !has:
			// Nothing to move. A connection loaded without its password,
			// which the keychain could not give, keeps its mark.
		case s.Secrets != nil && keychainErr == nil:
			if err := s.Secrets.Set(c.Name, password); err != nil {
				keychainErr, c.Secret = err, ""
			} else {
				c.DSN, c.Secret = clean, secretInKeychain
			}
		default:
			c.Secret = "" // no keychain: the password stays in DSN
		}
		out[i] = c
	}
	if err := s.write(out); err != nil {
		return err
	}

	if s.Secrets != nil {
		kept := map[string]bool{}
		for _, c := range out {
			if c.Secret == secretInKeychain {
				kept[c.Name] = true
			}
		}
		for _, c := range before {
			if c.Secret == secretInKeychain && !kept[c.Name] {
				_ = s.Secrets.Delete(c.Name)
			}
		}
	}
	if keychainErr != nil {
		return &KeychainError{keychainErr}
	}
	return nil
}

// MoveToKeychain moves passwords still in the file into the keychain, as
// on the first run of a rimor that has keychain support.
func (s Store) MoveToKeychain() error {
	if s.Secrets == nil {
		return nil
	}
	conns, err := s.read()
	if err != nil {
		return err
	}
	for _, c := range conns {
		if _, _, ok := splitPassword(c.Driver, c.DSN); ok {
			return s.Save(conns)
		}
	}
	return nil
}

func (s Store) write(conns []Config) error {
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
