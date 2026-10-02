package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFirstRunWritesTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rimor", "config.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Icons != IconsAuto || s.Leader != "ctrl+g" {
		t.Errorf("defaults = %+v", s)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "# full_screen = ['alt+f', 'leader f']") {
		t.Fatalf("template:\n%s\n%v", data, err)
	}

	// The written file loads back to the defaults.
	again, err := Load(path)
	if err != nil || again.Icons != s.Icons || len(again.Keys) != 0 {
		t.Fatalf("reload = %+v, %v", again, err)
	}
}

func TestUncommentedTemplateLinesParse(t *testing.T) {
	// Uncommenting every line of the template gives a valid file whose
	// bindings are the defaults.
	var lines []string
	for _, l := range strings.Split(Template(), "\n") {
		if strings.HasPrefix(l, "# ") && strings.Contains(l, " = ") {
			l = strings.TrimPrefix(l, "# ")
		}
		lines = append(lines, l)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range Actions {
		if got := s.Bindings()[a.Name]; !slices.Equal(got, a.Defaults) {
			t.Errorf("%s = %v, want %v", a.Name, got, a.Defaults)
		}
	}
}

func TestOverridesAndErrors(t *testing.T) {
	write := func(content string) string {
		path := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(path, []byte(content), 0o600)
		return path
	}

	s, err := Load(write("icons = \"plain\"\nleader = \"ctrl+b\"\n[keys]\nfull_screen = [\"ctrl+z\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Icons != IconsPlain || s.Leader != "ctrl+b" || !slices.Equal(s.Bindings()["full_screen"], []string{"ctrl+z"}) {
		t.Errorf("overrides = %+v", s)
	}
	if !slices.Equal(s.Bindings()["quit"], []string{"ctrl+q"}) {
		t.Errorf("untouched action lost its default")
	}

	for content, want := range map[string]string{
		`icons = "fancy"`:                "icons must be",
		`colour = "red"`:                 "unknown setting",
		"[keys]\nzoom = [\"alt+z\"]":     "unknown action",
		"[keys]\nnew_tab = [\"ctrl+q\"]": "bound to both",
		"[keys]\nnew_tab = [\"ctrl+g\"]": "leader key",
		`leader = "ctrl+g x"`:            "single key",
		"icons = ":                       "config.toml",
	} {
		if _, err := Load(write(content)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", content, err, want)
		}
	}
}
