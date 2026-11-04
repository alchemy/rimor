package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeAgent is the agent's end of a connection: it reads what the client
// sends and writes raw JSON lines back.
type fakeAgent struct {
	t   *testing.T
	in  *bufio.Scanner
	out io.WriteCloser
}

func connect(t *testing.T, h Handlers) (*Conn, *fakeAgent) {
	t.Helper()
	clientR, agentW := io.Pipe()
	agentR, clientW := io.Pipe()
	c := NewConn(clientR, clientW, h)
	t.Cleanup(func() { agentW.Close(); clientW.Close() })
	return c, &fakeAgent{t: t, in: bufio.NewScanner(agentR), out: agentW}
}

// next reads the client's next message.
func (a *fakeAgent) next() message {
	a.t.Helper()
	if !a.in.Scan() {
		a.t.Fatal("the client sent nothing")
	}
	var m message
	if err := json.Unmarshal(a.in.Bytes(), &m); err != nil {
		a.t.Fatalf("%s: %v", a.in.Bytes(), err)
	}
	return m
}

func (a *fakeAgent) write(line string) {
	a.t.Helper()
	if _, err := io.WriteString(a.out, line+"\n"); err != nil {
		a.t.Fatal(err)
	}
}

func TestHandshakeAndSession(t *testing.T) {
	c, agent := connect(t, Handlers{})
	ctx := context.Background()

	done := make(chan InitializeResponse, 1)
	go func() {
		resp, err := c.Initialize(ctx, Implementation{Name: "rimor", Version: "test"})
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	m := agent.next()
	if m.Method != "initialize" {
		t.Fatalf("method %q", m.Method)
	}
	var p struct {
		ProtocolVersion int `json:"protocolVersion"`
		Caps            struct {
			FS       map[string]bool `json:"fs"`
			Terminal bool            `json:"terminal"`
		} `json:"clientCapabilities"`
		Info Implementation `json:"clientInfo"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.ProtocolVersion != 1 || p.Caps.Terminal || p.Caps.FS["readTextFile"] || p.Caps.FS["writeTextFile"] || p.Info.Name != "rimor" {
		t.Errorf("initialize params: %s", m.Params)
	}
	agent.write(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":1,"agentInfo":{"name":"fake","title":"Fake Agent","version":"1"}}}`)
	if resp := <-done; resp.AgentInfo == nil || resp.AgentInfo.Label() != "Fake Agent" {
		t.Errorf("agent info: %+v", resp.AgentInfo)
	}

	sid := make(chan string, 1)
	go func() {
		id, err := c.NewSession(ctx, NewSessionRequest{Cwd: "/tmp", McpServers: []McpServer{{Name: "rimor", Command: "/bin/rimor", Args: []string{"mcp"}}}})
		if err != nil {
			t.Error(err)
		}
		sid <- id
	}()
	m = agent.next()
	// stdio MCP servers carry args and env, even empty, and no type.
	if m.Method != "session/new" || !strings.Contains(string(m.Params), `"mcpServers":[{"name":"rimor","command":"/bin/rimor","args":["mcp"],"env":null}]`) &&
		!strings.Contains(string(m.Params), `"env":[]`) {
		t.Errorf("session/new: %s", m.Params)
	}
	agent.write(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"sessionId":"s1"}}`)
	if got := <-sid; got != "s1" {
		t.Errorf("session %q", got)
	}
}

func TestVersionMismatch(t *testing.T) {
	c, agent := connect(t, Handlers{})
	errc := make(chan error, 1)
	go func() {
		_, err := c.Initialize(context.Background(), Implementation{Name: "rimor", Version: "test"})
		errc <- err
	}()
	m := agent.next()
	agent.write(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":2}}`)
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("err = %v", err)
	}
}

// Updates arrive in order, before the prompt's response.
func TestPromptStreamsUpdates(t *testing.T) {
	var got []string
	c, agent := connect(t, Handlers{Update: func(sid string, u Update) {
		switch u.Kind {
		case UpdateAgentMessage:
			got = append(got, sid+":"+u.Chunk().Text)
		case UpdateToolCall:
			got = append(got, sid+":tool "+*u.Title+" "+*u.Status)
		case UpdateToolCallEdit:
			got = append(got, sid+":done "+*u.Status+" "+u.ToolContent())
		}
	}})
	stop := make(chan string, 1)
	go func() {
		reason, err := c.Prompt(context.Background(), "s1", []ContentBlock{Text("hi")})
		if err != nil {
			t.Error(err)
		}
		stop <- reason
	}()
	m := agent.next()
	if m.Method != "session/prompt" || string(m.Params) != `{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}]}` {
		t.Fatalf("prompt: %s %s", m.Method, m.Params)
	}
	agent.write(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hel"}}}}`)
	agent.write(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"lo"}}}}`)
	agent.write(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"describe","status":"pending"}}}`)
	agent.write(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"3 columns"}}]}}}`)
	agent.write(`not json: a stray log line`)
	agent.write(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"stopReason":"end_turn"}}`)
	if r := <-stop; r != StopEndTurn {
		t.Errorf("stop reason %q", r)
	}
	want := []string{"s1:Hel", "s1:lo", "s1:tool describe pending", "s1:done completed 3 columns"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("updates %q, want %q", got, want)
	}
}

func TestPermissionRequests(t *testing.T) {
	asked := make(chan PermissionRequest, 1)
	c, agent := connect(t, Handlers{Permission: func(p PermissionRequest) string {
		asked <- p
		if strings.HasPrefix(*p.ToolCall.Name, "mcp__rimor__") {
			return "allow"
		}
		return ""
	}})
	_ = c

	agent.write(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"t1","name":"mcp__rimor__describe","title":"describe"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"no","name":"Reject","kind":"reject_once"}]}}`)
	if p := <-asked; len(p.Options) != 2 || p.Options[0].Kind != "allow_once" {
		t.Errorf("request: %+v", p)
	}
	if m := agent.next(); string(m.ID) != "7" || string(m.Result) != `{"outcome":{"optionId":"allow","outcome":"selected"}}` {
		t.Errorf("answer: %s %s", m.ID, m.Result)
	}

	agent.write(`{"jsonrpc":"2.0","id":"x","method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"t2","name":"Bash","title":"rm -rf /"},"options":[]}}`)
	<-asked
	if m := agent.next(); string(m.ID) != `"x"` || string(m.Result) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Errorf("answer: %s %s", m.ID, m.Result)
	}

	// No file system or terminal for the agent.
	agent.write(`{"jsonrpc":"2.0","id":8,"method":"fs/read_text_file","params":{"sessionId":"s1","path":"/etc/passwd"}}`)
	if m := agent.next(); m.Error == nil || m.Error.Code != codeNoMethod || m.Result != nil {
		t.Errorf("fs answer: %+v", m)
	}
}

func TestErrorsAndClose(t *testing.T) {
	c, agent := connect(t, Handlers{})
	errc := make(chan error, 1)
	go func() {
		_, err := c.NewSession(context.Background(), NewSessionRequest{Cwd: "/"})
		errc <- err
	}()
	m := agent.next()
	agent.write(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"error":{"code":-32000,"message":"Authentication required"}}`)
	if err := <-errc; !IsAuthRequired(err) {
		t.Errorf("err = %v", err)
	}

	// A call waiting when the agent exits fails instead of hanging.
	go func() {
		_, err := c.Prompt(context.Background(), "s1", []ContentBlock{Text("hi")})
		errc <- err
	}()
	agent.next()
	agent.out.Close()
	select {
	case err := <-errc:
		if err != ErrClosed {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt did not return")
	}
	<-c.Done()
	if err := c.Cancel("s1"); err != ErrClosed {
		t.Errorf("cancel after close: %v", err)
	}
	if _, err := c.Prompt(context.Background(), "s1", nil); err != ErrClosed {
		t.Errorf("prompt after close: %v", err)
	}
}

func TestStartMissingCommand(t *testing.T) {
	_, err := Start([]string{"rimor-no-such-agent-acp"}, nil, t.TempDir(), Handlers{})
	if !errors.Is(err, ErrNotInstalled) || err.Error() != "rimor-no-such-agent-acp: not installed (or not in PATH)" {
		t.Errorf("err = %v", err)
	}
}
