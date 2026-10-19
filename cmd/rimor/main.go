package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/agent"
	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
	"rimor.dev/internal/ui"
	"rimor.dev/internal/update"
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

// goInstalled reports a binary built by go install, which records the
// module version that release builds set themselves.
func goInstalled() bool {
	if version != "" {
		return false
	}
	info, ok := debug.ReadBuildInfo()
	return ok && info.Main.Version != "" && info.Main.Version != "(devel)"
}

// updatePath is where the last update check is kept, beside the session.
func updatePath() string {
	session, err := ui.DefaultSessionPath()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(session), "update.json")
}

// release describes a newer release for the UI.
func release(s update.State, announce bool) ui.UpdateMsg {
	msg := ui.UpdateMsg{Latest: s.Latest, URL: s.URL, Announce: announce}
	if goInstalled() {
		msg.Command = "go install rimor.dev/cmd/rimor@latest"
	}
	return msg
}

// checkUpdates shows what the last check found and, when a day has passed,
// asks again in the background; failures are silent.
func checkUpdates(m ui.Model, enabled bool) (ui.Model, func(p *tea.Program)) {
	path := updatePath()
	if !enabled || path == "" || update.Packaged() {
		return m, nil
	}
	s := update.Load(path)
	if update.Newer(Version(), s.Latest) {
		announce := s.Notified != s.Latest
		m = m.WithUpdate(release(s, announce))
		if announce {
			s.Notified = s.Latest
			_ = s.Save(path)
		}
	}
	if !s.Due(time.Now()) || Version() == "dev" {
		return m, nil
	}
	return m, func(p *tea.Program) {
		checked, err := update.Check(context.Background(), http.DefaultClient, Version(), s, time.Now())
		if err != nil {
			return
		}
		newer := update.Newer(Version(), checked.Latest)
		announce := newer && checked.Notified != checked.Latest
		if announce {
			checked.Notified = checked.Latest
		}
		_ = checked.Save(path)
		if newer {
			p.Send(release(checked, announce))
		}
	}
}

func main() {
	// rimor mcp: the MCP server an AI agent starts, which talks to the
	// rimor the user has open.
	if len(os.Args) == 2 && os.Args[1] == "mcp" {
		if err := agent.RunMCP(os.Stdin, os.Stdout, Version()); err != nil {
			fmt.Fprintln(os.Stderr, "rimor mcp:", err)
			os.Exit(1)
		}
		return
	}

	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "rimor %s, a terminal workbench for SQL databases\n\n"+
			"Usage:\n  rimor [-version]\n  rimor mcp    MCP server for AI agents, on stdin and stdout\n", Version())
	}
	flag.Parse()
	if *showVersion {
		line := "rimor " + Version()
		if path := updatePath(); path != "" {
			if s := update.Load(path); update.Newer(Version(), s.Latest) {
				line += " (" + s.Latest + " is out: " + s.URL + ")"
			}
		}
		fmt.Println(line)
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
	// Passwords saved before keychain support move there now.
	moved := store.MoveToKeychain()
	m := ui.New(store, sessionPath, settings)
	if moved != nil {
		m = m.WithStoreError(moved)
	}
	var srv *agent.Server
	if settings.Agent {
		// Without the socket rimor still works; agents just cannot reach it.
		if srv, err = agent.Listen(); err != nil {
			m = m.WithAgentError(err)
		} else {
			defer srv.Close()
		}
	}
	m, check := checkUpdates(m, settings.CheckUpdates)
	p := tea.NewProgram(m)
	if srv != nil {
		go srv.Serve(forward(p))
	}
	if check != nil {
		go check(p)
	}
	final, err := p.Run()
	if m, ok := final.(ui.Model); ok {
		m.Close()
	}
	return err
}

// forward hands agent tool calls to the UI, which owns the connections
// and tabs, and waits for its answer.
func forward(p *tea.Program) agent.Handler {
	return func(ctx context.Context, tool string, args json.RawMessage) (any, error) {
		reply := make(chan ui.AgentReply, 1)
		go p.Send(ui.AgentRequestMsg{Tool: tool, Args: args, Reply: reply})
		select {
		case r := <-reply:
			return r.Result, r.Err
		case <-ctx.Done():
			return nil, errors.New("rimor did not answer in time")
		}
	}
}
