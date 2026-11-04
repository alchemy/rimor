package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/acp"
	"rimor.dev/internal/agent"
	"rimor.dev/internal/config"
	"rimor.dev/internal/paths"
)

// A chat session is one agent process and one ACP session in it. What the
// agent sends arrives on the agent's goroutines; the session turns it into
// messages on one channel, which the UI reads one at a time (next), so
// updates keep their order and the end of a turn comes after its updates.

// startTimeout bounds starting the agent and opening its session; npx may
// download the adapter on first use.
const startTimeout = 2 * time.Minute

// chatSession is a running agent.
type chatSession struct {
	events chan tea.Msg
	closed chan struct{}
	once   sync.Once

	mu     sync.Mutex
	conn   *acp.Conn
	stop   func()        // ends the agent's process
	stderr func() string // the end of its stderr
	id     string        // the ACP session
}

// Messages from a session to the UI. Each names its session, so that one
// from a session the user has left is dropped.
type (
	chatReadyMsg struct {
		sess  *chatSession
		label string // the agent's name and version
		err   error
	}
	chatUpdateMsg struct {
		sess *chatSession
		u    acp.Update
	}
	chatPermissionMsg struct {
		sess  *chatSession
		req   acp.PermissionRequest
		reply chan<- string // the chosen option, "" to cancel
	}
	chatTurnMsg struct {
		sess *chatSession
		stop string
		err  error
	}
	chatExitMsg struct {
		sess   *chatSession
		err    error
		stderr string
	}
)

// chatMsg is any of the session messages.
type chatMsg interface{ session() *chatSession }

func (m chatReadyMsg) session() *chatSession      { return m.sess }
func (m chatUpdateMsg) session() *chatSession     { return m.sess }
func (m chatPermissionMsg) session() *chatSession { return m.sess }
func (m chatTurnMsg) session() *chatSession       { return m.sess }
func (m chatExitMsg) session() *chatSession       { return m.sess }

// startFunc starts an agent's process in dir; tests replace it.
type startFunc func(a config.ChatAgent, dir string, h acp.Handlers) (conn *acp.Conn, stop func(), stderr func() string, err error)

func startProcess(a config.ChatAgent, dir string, h acp.Handlers) (*acp.Conn, func(), func() string, error) {
	p, err := acp.Start(a.Command, a.Env, dir, h)
	if err != nil {
		return nil, nil, nil, err
	}
	return p.Conn, p.Close, p.Stderr, nil
}

func newChatSession() *chatSession {
	return &chatSession{events: make(chan tea.Msg, 64), closed: make(chan struct{})}
}

// next waits for the session's next message.
func (s *chatSession) next() tea.Cmd {
	return func() tea.Msg {
		select {
		case m := <-s.events:
			return m
		case <-s.closed:
			return nil
		}
	}
}

func (s *chatSession) emit(m tea.Msg) {
	select {
	case s.events <- m:
	case <-s.closed:
	}
}

// close ends the agent. It may wait for the process, so call it off the
// UI's goroutine.
func (s *chatSession) close() {
	s.once.Do(func() {
		close(s.closed)
		s.mu.Lock()
		stop := s.stop
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
	})
}

func (s *chatSession) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *chatSession) connection() (*acp.Conn, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn, s.id
}

// lockdown is the session's _meta: Claude's adapter takes SDK options
// there. tools: [] turns off Claude Code's built-in tools (shell, files,
// web), strict-mcp-config leaves out the user's own MCP servers, and
// rimor's tools need no confirmation. Other agents ignore it.
var lockdown = map[string]any{
	"claudeCode": map[string]any{"options": map[string]any{
		"tools":        []string{},
		"allowedTools": []string{"mcp__rimor"},
		"extraArgs":    map[string]any{"strict-mcp-config": nil},
	}},
	"disableBuiltInTools": true,
}

// begin starts the agent and opens a session whose only MCP server is
// rimor mcp, pointed at this rimor's socket. It reports with a
// chatReadyMsg.
func (s *chatSession) begin(a config.ChatAgent, socket, version string, start startFunc) {
	fail := func(err error) { s.emit(chatReadyMsg{sess: s, err: err}) }
	exe, err := os.Executable()
	if err != nil {
		fail(err)
		return
	}
	dir, err := chatDir()
	if err != nil {
		fail(err)
		return
	}
	h := acp.Handlers{
		Update: func(_ string, u acp.Update) { s.emit(chatUpdateMsg{s, u}) },
		Permission: func(p acp.PermissionRequest) string {
			reply := make(chan string, 1)
			s.emit(chatPermissionMsg{s, p, reply})
			select {
			case r := <-reply:
				return r
			case <-s.closed:
				return ""
			}
		},
	}
	conn, stop, stderr, err := start(a, dir, h)
	if err != nil {
		fail(err)
		return
	}
	s.mu.Lock()
	s.conn, s.stop, s.stderr = conn, stop, stderr
	s.mu.Unlock()
	if s.isClosed() { // left while starting
		stop()
		return
	}
	go func() {
		<-conn.Done()
		s.emit(chatExitMsg{sess: s, err: conn.Err(), stderr: stderr()})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	init, err := conn.Initialize(ctx, acp.Implementation{Name: "rimor", Title: "rimor", Version: version})
	if err != nil {
		fail(withStderr(err, stderr()))
		return
	}
	id, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: dir,
		McpServers: []acp.McpServer{{
			Name: "rimor", Command: exe, Args: []string{"mcp"},
			Env: []acp.EnvVariable{{Name: agent.SocketEnv, Value: socket}},
		}},
		Meta: lockdown,
	})
	if err != nil {
		if acp.IsAuthRequired(err) {
			err = loginError(a, init.AuthMethods)
		}
		fail(withStderr(err, stderr()))
		return
	}
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
	label := a.Label()
	if info := init.AgentInfo; info != nil {
		label = info.Label() + " " + info.Version
	}
	s.emit(chatReadyMsg{sess: s, label: label})
}

// prompt sends the user's message; the turn's end comes as a chatTurnMsg.
func (s *chatSession) prompt(blocks []acp.ContentBlock) {
	conn, id := s.connection()
	stop, err := conn.Prompt(context.Background(), id, blocks)
	s.emit(chatTurnMsg{s, stop, err})
}

// cancel asks the agent to end its turn.
func (s *chatSession) cancel() {
	if conn, id := s.connection(); conn != nil {
		_ = conn.Cancel(id)
	}
}

// chatDir is the agents' working directory: empty, so that an agent
// that can still read files in its working directory finds nothing there.
func chatDir() (string, error) {
	state, err := paths.State()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(state, "agent")
	return dir, os.MkdirAll(dir, 0o700)
}

// loginError explains how to log in to an agent that needs it.
func loginError(a config.ChatAgent, methods []acp.AuthMethod) error {
	msg := a.Label() + " needs you to log in. Log in with its own CLI in a terminal"
	switch a.Name {
	case "claude":
		msg += " (claude, then /login)"
	case "codex":
		msg += " (codex login)"
	case "gemini":
		msg += " (gemini)"
	}
	msg += ", then press ctrl+n here to start again."
	var names []string
	for _, m := range methods {
		names = append(names, m.Name)
	}
	if len(names) > 0 {
		msg += " It offers: " + strings.Join(names, ", ") + "."
	}
	return errors.New(msg)
}

// withStderr adds the last lines the agent wrote on stderr to an error.
func withStderr(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	lines := strings.Split(stderr, "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return errors.New(err.Error() + "\n" + strings.Join(lines, "\n"))
}

// ── Tool calls ──────────────────────────────────────────────────────────────

// rimorTool returns the rimor tool a tool call runs, or "" when it runs
// anything else. Each adapter names MCP tool calls its own way: Claude's
// mcp__rimor__describe, Codex's mcp.rimor.describe (and "MCP describe on
// rimor" when it asks permission), Gemini's "describe (rimor MCP Server)". Calls that run commands or change files
// never count, whatever they are called.
func rimorTool(u acp.Update) string {
	if u.ToolKind != nil {
		switch *u.ToolKind {
		case "execute", "edit", "delete", "move", "fetch":
			return ""
		}
	}
	isTool := func(name string) bool {
		return slices.ContainsFunc(agent.Tools, func(t agent.Tool) bool { return t.Name == name })
	}
	for _, s := range []*string{u.Name, u.Title} {
		if s == nil {
			continue
		}
		name := strings.TrimSpace(*s)
		for _, pattern := range [][2]string{
			{"mcp__rimor__", ""},
			{"mcp.rimor.", ""},
			{"MCP ", " on rimor"},
			{"", " (rimor MCP Server)"},
			{"rimor/", ""},
			{"rimor.", ""},
			{"rimor__", ""},
		} {
			rest, ok := strings.CutPrefix(name, pattern[0])
			if !ok {
				continue
			}
			if rest, ok = strings.CutSuffix(rest, pattern[1]); ok && isTool(rest) {
				return rest
			}
		}
	}
	return ""
}

// permissionChoice answers a permission request: rimor's tools may run,
// since they neither execute SQL nor reveal credentials; anything else
// (a shell command, a file edit, another MCP server's tool) is refused.
func permissionChoice(p acp.PermissionRequest) (option string, allowed bool) {
	allowed = rimorTool(p.ToolCall) != ""
	kinds := []string{"reject_once", "reject_always"}
	if allowed {
		kinds = []string{"allow_always", "allow_once"}
	}
	for _, k := range kinds {
		for _, o := range p.Options {
			if o.Kind == k {
				return o.ID, allowed
			}
		}
	}
	return "", allowed // no matching option: cancel the call
}
