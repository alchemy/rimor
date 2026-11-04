package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/acp"
	"rimor.dev/internal/agent"
	"rimor.dev/internal/config"
)

// chatLoop drives the model as Bubble Tea does: commands run concurrently
// and their messages come back through one queue. The chat needs it: a
// session's messages arrive while other commands still run.
type chatLoop struct {
	*driver
	msgs chan tea.Msg
}

func (l *chatLoop) send(msg tea.Msg) {
	next, cmd := l.m.Update(msg)
	l.m = next.(Model)
	l.spawn(cmd)
}

func (l *chatLoop) spawn(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				l.spawn(c)
			}
			return
		}
		switch msg.(type) {
		case nil, spinner.TickMsg, cursor.BlinkMsg:
			return
		}
		l.msgs <- msg
	}()
}

func (l *chatLoop) key(keys ...string) {
	for _, k := range keys {
		l.send(keyMsg(l.t, k))
	}
}

func (l *chatLoop) typeText(s string) {
	for _, r := range s {
		l.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// until handles messages until cond holds.
func (l *chatLoop) until(what string, cond func() bool) {
	l.t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case msg := <-l.msgs:
			l.send(msg)
		case <-deadline:
			l.t.Fatalf("waiting for %s; the chat shows:\n%s", what, l.chat())
		}
	}
}

// chat is the conversation as plain text, at a width that keeps lines
// whole.
func (l *chatLoop) chat() string {
	c := l.m.chat
	c.SetSize(400, 200)
	var lines []string
	for _, line := range c.transcript() {
		lines = append(lines, strings.TrimRight(ansi.Strip(line), " "))
	}
	return strings.Join(lines, "\n")
}

// fakeAgent speaks ACP on the agent's side of a pipe. onPrompt scripts
// each turn; it runs on its own goroutine.
type fakeAgent struct {
	t        *testing.T
	loop     *chatLoop
	onPrompt func(a *fakeAgent, text string) string // returns the stop reason
	authFail bool

	wmu sync.Mutex
	enc *json.Encoder

	mu         sync.Mutex
	nextID     int
	waiting    map[int]chan json.RawMessage
	sessionNew json.RawMessage
	prompts    []string
	cancelled  chan struct{}
	stopped    chan struct{}
}

func newFakeAgent(t *testing.T, l *chatLoop, onPrompt func(a *fakeAgent, text string) string) *fakeAgent {
	a := &fakeAgent{t: t, loop: l, onPrompt: onPrompt, waiting: map[int]chan json.RawMessage{},
		cancelled: make(chan struct{}, 1), stopped: make(chan struct{})}
	l.m.chat.start = func(_ config.ChatAgent, _ string, h acp.Handlers) (*acp.Conn, func(), func() string, error) {
		clientR, agentW := io.Pipe()
		agentR, clientW := io.Pipe()
		a.enc = json.NewEncoder(agentW)
		go a.serve(agentR)
		var once sync.Once
		stop := func() {
			once.Do(func() {
				clientW.Close()
				agentW.Close()
				close(a.stopped)
			})
		}
		return acp.NewConn(clientR, clientW, h), stop, func() string { return "fake stderr" }, nil
	}
	return a
}

func (a *fakeAgent) write(v any) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	_ = a.enc.Encode(v)
}

func (a *fakeAgent) reply(id json.RawMessage, result any) {
	a.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (a *fakeAgent) serve(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			a.t.Errorf("fake agent: %s: %v", sc.Bytes(), err)
			return
		}
		switch m.Method {
		case "initialize":
			a.reply(m.ID, map[string]any{"protocolVersion": 1,
				"agentInfo":   map[string]any{"name": "fake", "title": "Fake Agent", "version": "1.0"},
				"authMethods": []map[string]any{{"id": "login", "name": "Log in with Fake"}}})
		case "session/new":
			a.mu.Lock()
			a.sessionNew = m.Params
			a.mu.Unlock()
			if a.authFail {
				a.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "Authentication required"}})
				continue
			}
			a.reply(m.ID, map[string]any{"sessionId": "s1"})
		case "session/prompt":
			var p struct {
				Prompt []acp.ContentBlock `json:"prompt"`
			}
			_ = json.Unmarshal(m.Params, &p)
			var texts []string
			for _, b := range p.Prompt {
				texts = append(texts, b.Text)
			}
			text := strings.Join(texts, "\n")
			a.mu.Lock()
			a.prompts = append(a.prompts, text)
			a.mu.Unlock()
			go func(id json.RawMessage) {
				a.reply(id, map[string]any{"stopReason": a.onPrompt(a, text)})
			}(m.ID)
		case "session/cancel":
			a.cancelled <- struct{}{}
		case "":
			var id int
			_ = json.Unmarshal(m.ID, &id)
			a.mu.Lock()
			ch := a.waiting[id]
			a.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
		}
	}
}

func (a *fakeAgent) update(u map[string]any) {
	a.write(map[string]any{"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": "s1", "update": u}})
}

func (a *fakeAgent) say(text string) {
	a.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
}

// askPermission asks rimor whether a tool call may run, and returns the
// option it chose, or "" for cancelled.
func (a *fakeAgent) askPermission(name, title, kind string) string {
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	ch := make(chan json.RawMessage, 1)
	a.waiting[id] = ch
	a.mu.Unlock()
	a.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission",
		"params": map[string]any{"sessionId": "s1",
			"toolCall": map[string]any{"toolCallId": "p" + name, "name": name, "title": title, "kind": kind},
			"options": []map[string]any{
				{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
				{"optionId": "always", "name": "Always allow", "kind": "allow_always"},
				{"optionId": "no", "name": "Reject", "kind": "reject_once"},
			}}})
	var res struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(<-ch, &res)
	return res.Outcome.OptionID
}

// callRimor makes a tool call the way rimor mcp does, through the socket
// handler.
func (a *fakeAgent) callRimor(tool, args string) (any, error) {
	reply := make(chan AgentReply, 1)
	a.loop.msgs <- AgentRequestMsg{Tool: tool, Args: json.RawMessage(args), Reply: reply}
	r := <-reply
	return r.Result, r.Err
}

// chatDriver opens rimor on the agent tests' "ERP" database, with agents
// able to reach it.
func chatDriver(t *testing.T) *chatLoop {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // the agents' working directory
	d := agentDriver(t)
	d.m = d.m.WithAgentSocket("/run/rimor/42.sock", "v-test")
	return &chatLoop{driver: d, msgs: make(chan tea.Msg, 64)}
}

func TestChatConversation(t *testing.T) {
	l := chatDriver(t)
	var choices []string
	a := newFakeAgent(t, l, func(a *fakeAgent, text string) string {
		a.update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "The user wants overdue orders."}})
		a.say("Looking at the ")
		a.say("orders table.")
		// Claude's adapter sends the input once it is complete, in an update.
		a.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "name": "mcp__rimor__open_query",
			"title": "mcp__rimor__open_query", "status": "pending", "kind": "other", "rawInput": map[string]any{}})
		a.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1",
			"rawInput": map[string]any{"connection": "ERP", "sql": "SELECT 1", "title": "overdue"}})
		choices = append(choices, a.askPermission("mcp__rimor__open_query", "open_query", "other"))
		if _, err := a.callRimor("open_query", `{"connection":"ERP","sql":"SELECT * FROM orders WHERE delivery_date < date('now')","title":"overdue"}`); err != nil {
			t.Error(err)
		}
		a.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed"})
		a.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t2", "title": "MCP describe on rimor", "status": "in_progress"})
		a.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t2", "status": "failed",
			"content": []map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": "no table nope\nmore"}}}})
		// Anything else is refused, whatever it claims to be.
		choices = append(choices, a.askPermission("Bash", "rm -rf ~", "execute"))
		choices = append(choices, a.askPermission("Bash", "echo mcp__rimor__describe", "execute"))
		a.say("\n\nI opened the query \x1b[31min a tab\x1b[0m: review it and run it.")
		return acp.StopEndTurn
	})

	if l.m.chat.sess != nil {
		t.Fatal("an agent started before the chat had the focus")
	}
	l.key("alt+c")
	if l.m.focus != focusChat || l.m.chat.phase != chatStarting {
		t.Fatalf("alt+c: focus %v, phase %v", l.m.focus, l.m.chat.phase)
	}
	l.until("the agent to connect", func() bool { return l.m.chat.phase == chatReady })
	if got := l.chat(); !strings.Contains(got, "Connected to Fake Agent 1.0") {
		t.Errorf("after connecting:\n%s", got)
	}

	// The session gets rimor mcp, pointed at this rimor, and nothing else.
	var sn struct {
		Cwd        string          `json:"cwd"`
		McpServers []acp.McpServer `json:"mcpServers"`
		Meta       struct {
			ClaudeCode struct {
				Options struct {
					Tools        []string `json:"tools"`
					AllowedTools []string `json:"allowedTools"`
				} `json:"options"`
			} `json:"claudeCode"`
		} `json:"_meta"`
	}
	a.mu.Lock()
	if err := json.Unmarshal(a.sessionNew, &sn); err != nil {
		t.Fatal(err)
	}
	a.mu.Unlock()
	if len(sn.McpServers) != 1 || sn.McpServers[0].Name != "rimor" || strings.Join(sn.McpServers[0].Args, " ") != "mcp" ||
		len(sn.McpServers[0].Env) != 1 || sn.McpServers[0].Env[0] != (acp.EnvVariable{Name: agent.SocketEnv, Value: "/run/rimor/42.sock"}) {
		t.Errorf("mcp servers: %+v", sn.McpServers)
	}
	if sn.Meta.ClaudeCode.Options.Tools == nil || len(sn.Meta.ClaudeCode.Options.Tools) != 0 ||
		strings.Join(sn.Meta.ClaudeCode.Options.AllowedTools, ",") != "mcp__rimor" {
		t.Errorf("claude lockdown: %+v", sn.Meta)
	}
	if !strings.HasSuffix(sn.Cwd, "agent") {
		t.Errorf("cwd %q", sn.Cwd)
	}

	l.typeText("overdue orders in ERP")
	l.key("enter")
	if l.m.chat.phase != chatBusy || l.m.chat.input.Text() != "" {
		t.Errorf("after enter: phase %v, input %q", l.m.chat.phase, l.m.chat.input.Text())
	}
	l.until("the turn to end", func() bool { return l.m.chat.phase == chatReady })

	// The prompt tells the agent where the user is.
	a.mu.Lock()
	prompt := a.prompts[0]
	a.mu.Unlock()
	if !strings.HasPrefix(prompt, "overdue orders in ERP\n(From rimor, not typed by the user: the user is on query tab 1, \"untitled-1\"") {
		t.Errorf("prompt %q", prompt)
	}

	if strings.Join(choices, ",") != "always,no,no" {
		t.Errorf("permission choices %v, want always for rimor's tool, no for the rest", choices)
	}
	// The agent's tab opened beside the chat, which keeps the focus.
	if cur := l.m.query.current(); !cur.agent || !strings.Contains(cur.ed.Text(), "FROM orders") || l.m.focus != focusChat {
		t.Errorf("tab %q, agent %v, focus %v", cur.title(), cur.agent, l.m.focus)
	}
	got := l.chat()
	for _, want := range []string{
		"› overdue orders in ERP",
		"The user wants overdue orders.",
		"Looking at the orders table.",
		"✓ rimor › open_query  ERP · overdue",
		"✗ rimor › describe: no table nope",
		"Refused: rm -rf ~. The chat allows rimor's tools only.",
		"Refused: echo mcp__rimor__describe.",
		"I opened the query in a tab: review it and run it.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("chat lacks %q:\n%s", want, got)
		}
	}
	for _, e := range l.m.chat.entries {
		if strings.ContainsAny(e.text, "\x1b\r") {
			t.Errorf("escape sequences from the agent reach the screen: %q", e.text)
		}
	}
	if screen := l.screen(); !strings.Contains(screen, "4  Agent · Fake Agent 1.0") {
		t.Errorf("pane title:\n%s", screen)
	}
}

func TestChatStopAndNewChat(t *testing.T) {
	l := chatDriver(t)
	newFakeAgent(t, l, func(a *fakeAgent, text string) string {
		a.say("Thinking hard…")
		<-a.cancelled
		return acp.StopCancelled
	})
	l.key("alt+4")
	l.until("the agent to connect", func() bool { return l.m.chat.phase == chatReady })
	l.typeText("slow question")
	l.key("enter")
	l.until("the answer to start", func() bool { return strings.Contains(l.chat(), "Thinking hard") })
	if !strings.Contains(ansi.Strip(l.m.chat.Status()), "working") {
		t.Errorf("status %q", ansi.Strip(l.m.chat.Status()))
	}
	l.key("esc")
	l.until("the turn to stop", func() bool { return l.m.chat.phase == chatReady })
	if !strings.Contains(l.chat(), "Stopped.") {
		t.Errorf("after esc:\n%s", l.chat())
	}

	// ctrl+n leaves the agent and offers the agents again.
	old := l.m.chat.sess
	l.key("ctrl+n")
	if l.m.chat.phase != chatPicking || l.m.chat.sess != nil {
		t.Fatalf("ctrl+n: phase %v", l.m.chat.phase)
	}
	select {
	case <-old.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the old session was not closed")
	}
	if v := ansi.Strip(l.m.chat.View()); !strings.Contains(v, "› Claude") || !strings.Contains(v, "codex-acp") {
		t.Errorf("picker:\n%s", v)
	}
	l.key("down", "enter")
	if l.m.chat.agent.Name != "codex" || l.m.chat.phase != chatStarting {
		t.Errorf("picked %q, phase %v", l.m.chat.agent.Name, l.m.chat.phase)
	}
	l.until("codex to connect", func() bool { return l.m.chat.phase == chatReady })
}

func TestChatFailures(t *testing.T) {
	l := chatDriver(t)
	l.m.chat.start = func(a config.ChatAgent, _ string, _ acp.Handlers) (*acp.Conn, func(), func() string, error) {
		return nil, nil, nil, fmt.Errorf("%s: %w", a.Command[0], acp.ErrNotInstalled)
	}
	l.key("alt+c")
	l.until("the failure", func() bool { return l.m.chat.phase == chatFailed })
	if got := strings.Join(strings.Fields(l.chat()), " "); !strings.Contains(got, "Could not start Claude: claude-agent-acp: not installed (or not in PATH). "+
		"Install it with: npm install -g @agentclientprotocol/claude-agent-acp (or set its command in config.toml)") {
		t.Errorf("not installed:\n%s", got)
	}

	// An agent that needs a login says how to log in.
	a := newFakeAgent(t, l, nil)
	a.authFail = true
	l.key("ctrl+n", "enter")
	l.until("the login error", func() bool { return l.m.chat.phase == chatFailed })
	if got := l.chat(); !strings.Contains(got, "Claude needs you to log in") || !strings.Contains(got, "claude, then /login") ||
		!strings.Contains(got, "It offers: Log in with Fake.") {
		t.Errorf("login:\n%s", got)
	}
	select {
	case <-a.stopped:
	case <-time.After(5 * time.Second):
		t.Error("the failed agent was not stopped")
	}

	// An agent that exits mid-conversation.
	a = newFakeAgent(t, l, func(a *fakeAgent, text string) string {
		a.loop.m.chat.sess.stop() // the process dies
		return acp.StopEndTurn
	})
	l.key("ctrl+n", "enter")
	l.until("the agent to connect", func() bool { return l.m.chat.phase == chatReady })
	sess := l.m.chat.sess
	l.typeText("hi")
	l.key("enter")
	l.until("the exit", func() bool { return l.m.chat.phase == chatFailed })
	if got := l.chat(); !strings.Contains(got, "Claude stopped: the agent stopped") || !strings.Contains(got, "fake stderr") {
		t.Errorf("exit:\n%s", got)
	}
	select {
	case <-sess.closed: // closed off the UI's goroutine
	case <-time.After(5 * time.Second):
		t.Error("the session of a stopped agent stays open")
	}
}

func TestChatWithoutAgentAccess(t *testing.T) {
	d := agentDriver(t) // no socket: agent = false, or it failed
	d.key("alt+c")
	if d.m.chat.phase != chatPicking || d.m.chat.sess != nil {
		t.Errorf("started without a socket")
	}
	if v := ansi.Strip(d.m.chat.View()); !strings.Contains(v, "Agent access is off") {
		t.Errorf("view:\n%s", v)
	}
	d.key("enter") // nothing to start
	if d.m.chat.sess != nil {
		t.Errorf("enter started an agent")
	}
}

func TestChatLayout(t *testing.T) {
	session := t.TempDir() + "/session.json"
	d := layoutDriver(t, session)

	// The chat is a pane like the others, on the right.
	chatW := d.m.chatWidth()
	if chatW != 36 { // 30% of 120
		t.Errorf("chat width %d", chatW)
	}
	if l, _, _ := d.m.layout(); l != 30 {
		t.Errorf("explorer %d", l)
	}
	screen := d.screen() // checks every row is 120 wide
	if !strings.Contains(screen, "4  Agent") || !strings.Contains(screen, "Agent access is off") {
		t.Errorf("no chat pane:\n%s", screen)
	}
	if d.m.paneAt(119, 5) != focusChat || d.m.paneAt(120-chatW-1, 5) != focusQuery {
		t.Errorf("paneAt")
	}
	d.setFocusForTest(focusResults)
	d.key("tab")
	if d.m.focus != focusChat {
		t.Errorf("tab from results: %v", d.m.focus)
	}
	d.key("tab")
	if d.m.focus != focusExplorer {
		t.Errorf("tab from the chat: %v", d.m.focus)
	}
	d.click(119, 5)
	if d.m.focus != focusChat || d.m.chat.sess != nil {
		t.Errorf("click: focus %v; an agent started without access", d.m.focus)
	}

	// Dragging the chat's border widens it, within limits.
	d.drag(120-chatW, 10, 120-chatW-10, 10)
	if got := d.m.chatWidth(); got != chatW+10 {
		t.Errorf("dragged to %d, want %d", got, chatW+10)
	}
	d.drag(120-d.m.chatWidth(), 10, 0, 10)
	if l, _, _ := d.m.layout(); l != minExplorerWidth || d.m.chatWidth() != 120-minExplorerWidth-minRightWidth {
		t.Errorf("dragged all the way: explorer %d, chat %d", l, d.m.chatWidth())
	}
	d.screen()
	d.drag(120-d.m.chatWidth(), 10, 119, 10)
	if got := d.m.chatWidth(); got != minChatWidth {
		t.Errorf("dragged narrow: %d", got)
	}
	d.key("alt+=")
	if d.m.chatWidth() != chatW {
		t.Errorf("reset: %d", d.m.chatWidth())
	}

	// The session remembers its width.
	d.drag(120-chatW, 10, 120-chatW-4, 10)
	d2 := layoutDriver(t, session)
	if d2.m.chatWidth() != chatW+4 {
		t.Errorf("restored width %d, want %d", d2.m.chatWidth(), chatW+4)
	}

	d.key("alt+shift+4")
	if !d.m.split.full || d.m.focus != focusChat {
		t.Errorf("full screen chat: full %v, focus %v", d.m.split.full, d.m.focus)
	}
	d.screen()
	d.key("4")
	if d.m.focus != focusChat {
		t.Errorf("4: focus %v", d.m.focus)
	}
}

func (d *driver) setFocusForTest(f focus) { d.m.setFocus(f) }

func TestRimorTool(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		name, title, kind *string
		want              string
	}{
		{name: s("mcp__rimor__describe"), want: "describe"}, // Claude
		{title: s("MCP open_query on rimor"), want: "open_query"},
		{title: s("mcp.rimor.wait_for_run"), want: "wait_for_run"},              // Codex
		{title: s("search_objects (rimor MCP Server)"), want: "search_objects"}, // Gemini
		{name: s("mcp__rimor__run_query"), want: ""},                            // not a rimor tool
		{name: s("mcp__other__describe"), want: ""},                             // another server
		{name: s("mcp__rimor__describe"), kind: s("execute"), want: ""},
		{title: s("echo MCP describe on rimor"), want: ""},
		{name: s("Bash"), title: s("rimor mcp"), want: ""},
	} {
		u := acp.Update{Name: c.name, Title: c.title, ToolKind: c.kind}
		if got := rimorTool(u); got != c.want {
			t.Errorf("%v %v %v: %q, want %q", deref(c.name), deref(c.title), deref(c.kind), got, c.want)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestPermissionChoiceWithoutMatchingOption(t *testing.T) {
	name := "mcp__rimor__describe"
	p := acp.PermissionRequest{ToolCall: acp.Update{Name: &name}, Options: []acp.PermissionOption{{ID: "x", Kind: "reject_once"}}}
	if opt, allowed := permissionChoice(p); opt != "" || !allowed {
		t.Errorf("no allow option: %q %v", opt, allowed)
	}
	bash := "Bash"
	p = acp.PermissionRequest{ToolCall: acp.Update{Name: &bash}, Options: []acp.PermissionOption{{ID: "y", Kind: "allow_once"}}}
	if opt, allowed := permissionChoice(p); opt != "" || allowed {
		t.Errorf("no reject option: %q %v", opt, allowed)
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("a\x1b]52;c;ZXZpbA==\x07b\tc\r\nd\x00\x1b[2Je"); got != "ab c\nde" {
		t.Errorf("sanitize = %q", got)
	}
}

// A notice wins over the status when the border has no room for both.
func TestNoticeBeforeStatus(t *testing.T) {
	p := pane{status: "◈ ERP · ✶ agent asks for results: alt+a", footer: "⚠ This query may write (DELETE) · review it", noticeFirst: true}
	bottom := func(p pane) string {
		lines := strings.Split(ansi.Strip(p.render("", 70, 3, false)), "\n")
		return lines[len(lines)-1]
	}
	if got := bottom(p); !strings.Contains(got, "⚠ This query may write (DELETE) · review it") || !strings.Contains(got, "◈ ERP · ✶ agent…") {
		t.Errorf("notice first: %q", got)
	}
	p.noticeFirst = false
	if got := bottom(p); strings.Contains(got, "⚠") || !strings.Contains(got, "agent asks for results: alt+a") {
		t.Errorf("status first: %q", got)
	}
	if got := bottom(pane{footer: strings.Repeat("x", 100), noticeFirst: true}); lipgloss.Width(got) != 70 || !strings.Contains(got, "x…") {
		t.Errorf("long notice: %q", got)
	}
}
