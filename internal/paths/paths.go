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
	"strconv"
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

// Runtime is the directory for the agent socket, private to the user:
// $XDG_RUNTIME_DIR/rimor where set, else in the temporary directory, which
// is per-user on macOS and Windows. Elsewhere the directory is named after
// the user and checked to be theirs alone (see private).
//
// Socket paths are limited to about 100 bytes, so a directory too deep for
// one falls back to the next choice, and finally to /tmp.
func Runtime() (string, error) {
	var dirs []string
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		dirs = append(dirs, filepath.Join(base, app))
	}
	switch runtime.GOOS {
	case "windows", "darwin":
		dirs = append(dirs, filepath.Join(os.TempDir(), app))
	default:
		dirs = append(dirs, filepath.Join(os.TempDir(), app+"-"+strconv.Itoa(os.Getuid())))
	}
	if runtime.GOOS != "windows" {
		dirs = append(dirs, filepath.Join("/tmp", app+"-"+strconv.Itoa(os.Getuid())))
	}
	for _, dir := range dirs {
		if len(dir) <= MaxSocketDir {
			return dir, nil
		}
	}
	return dirs[len(dirs)-1], nil
}

// MaxSocketDir is the longest directory that leaves room for a socket
// named after a process id: macOS allows 104 bytes in all.
const MaxSocketDir = 104 - len("/4294967295.sock") - 1

// MakeRuntime creates the runtime directory, readable by the user only,
// and returns it.
func MakeRuntime() (string, error) {
	dir, err := Runtime()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, private(dir)
}

// Home returns the user's home directory, or "" when it is unknown.
func Home() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// Omarchy reports whether this is an Omarchy desktop
// (https://omarchy.org): its session sets $OMARCHY_PATH, and installs live
// under ~/.local/share/omarchy.
func Omarchy() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("OMARCHY_PATH") != "" {
		return true
	}
	info, err := os.Stat(filepath.Join(Home(), ".local", "share", "omarchy"))
	return err == nil && info.IsDir()
}
