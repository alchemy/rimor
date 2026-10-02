// Package paths decides where rimor keeps its files on each platform.
//
//	            config (settings, connections)   state (session)
//	Linux/macOS ~/.config/rimor                  ~/.local/state/rimor
//	Windows     %APPDATA%\rimor                  %LOCALAPPDATA%\rimor
//
// $XDG_CONFIG_HOME and $XDG_STATE_HOME take precedence on every platform
// when set.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const app = "rimor"

// Config is the directory for settings and saved connections.
func Config() (string, error) {
	return dir("XDG_CONFIG_HOME", "APPDATA", ".config")
}

// State is the directory for the session.
func State() (string, error) {
	return dir("XDG_STATE_HOME", "LOCALAPPDATA", filepath.Join(".local", "state"))
}

func dir(xdgVar, windowsVar, unixRel string) (string, error) {
	if base := os.Getenv(xdgVar); base != "" {
		return filepath.Join(base, app), nil
	}
	if runtime.GOOS == "windows" {
		if base := os.Getenv(windowsVar); base != "" {
			return filepath.Join(base, app), nil
		}
		return "", errors.New("%" + windowsVar + "% is not set")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, unixRel, app), nil
}

// Home returns the user's home directory, or "" when it is unknown.
func Home() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
