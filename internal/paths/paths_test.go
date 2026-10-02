package paths

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestXDGOverrides(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	if got, _ := Config(); got != filepath.Join(base, "cfg", "rimor") {
		t.Errorf("Config = %q", got)
	}
	if got, _ := State(); got != filepath.Join(base, "state", "rimor") {
		t.Errorf("State = %q", got)
	}
}

func TestPlatformDefaults(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", base)        // Unix home
	t.Setenv("USERPROFILE", base) // Windows home
	t.Setenv("APPDATA", filepath.Join(base, "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(base, "Local"))

	wantConfig := filepath.Join(base, ".config", "rimor")
	wantState := filepath.Join(base, ".local", "state", "rimor")
	if runtime.GOOS == "windows" {
		wantConfig = filepath.Join(base, "Roaming", "rimor")
		wantState = filepath.Join(base, "Local", "rimor")
	}
	if got, err := Config(); err != nil || got != wantConfig {
		t.Errorf("Config = %q, %v; want %q", got, err, wantConfig)
	}
	if got, err := State(); err != nil || got != wantState {
		t.Errorf("State = %q, %v; want %q", got, err, wantState)
	}
}
