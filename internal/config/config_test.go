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

func TestChatAgents(t *testing.T) {
	names := func(s Settings) string {
		var out []string
		for _, a := range s.ChatAgentList() {
			out = append(out, a.Name+":"+strings.Join(a.Command, " "))
		}
		return strings.Join(out, ", ")
	}
	if got := names(Defaults()); got != "claude:claude-agent-acp, codex:codex-acp, gemini:gemini --acp" {
		t.Errorf("built in: %s", got)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`chat_agent = "zed"
chat_agents.claude = { command = ["npx", "-y", "@agentclientprotocol/claude-agent-acp"] }
chat_agents.gemini = { command = [] }
[chat_agents.zed]
title = "Zed agent"
command = ["zed-agent", "--acp"]
env = { ZED_MODEL = "large" }
`), 0o600)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(s); got != "claude:npx -y @agentclientprotocol/claude-agent-acp, codex:codex-acp, zed:zed-agent --acp" {
		t.Errorf("configured: %s", got)
	}
	list := s.ChatAgentList()
	if list[0].Label() != "claude" || list[1].Label() != "Codex" || list[2].Env["ZED_MODEL"] != "large" {
		t.Errorf("labels and env: %+v", list)
	}

	for content, want := range map[string]string{
		`chat_agent = "gemini"` + "\nchat_agents.gemini = { command = [] }": `chat_agent must be one of the chat agents ("claude", "codex"), not "gemini"`,
		`chat_agent = "nobody"`:                         "chat_agent must be one of",
		`chat_agents.x = { command = [" "] }`:           "[chat_agents.x]: empty command",
		`chat_agents.x = { command = ["a"], nope = 1 }`: "unknown setting",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(path, []byte(content), 0o600)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", content, err, want)
		}
	}
}
