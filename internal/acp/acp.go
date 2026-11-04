// Package acp is a client for the Agent Client Protocol, which editors use
// to talk to AI agents: Claude (through claude-agent-acp), Codex (through
// codex-acp), Gemini CLI (gemini --acp) and others.
//
// rimor starts the agent as a child process and speaks JSON-RPC 2.0 with
// it on the agent's stdin and stdout, one message per line. The client
// offers no file system and no terminal: the agent reaches rimor only
// through the MCP server it is given in NewSession, "rimor mcp".
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// ProtocolVersion is the ACP version this client speaks.
const ProtocolVersion = 1

// Error is a JSON-RPC error from the agent.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "agent error " + strconv.Itoa(e.Code)
	}
	return e.Message
}

// JSON-RPC and ACP error codes.
const (
	codeNoMethod      = -32601
	codeInvalidParams = -32602
	// CodeAuthRequired means the agent needs a login before a session.
	CodeAuthRequired = -32000
)

// IsAuthRequired reports an agent that needs the user to log in.
func IsAuthRequired(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == CodeAuthRequired
}

// ErrClosed means the agent's output ended: it exited or was stopped.
var ErrClosed = errors.New("the agent stopped")

// ── Messages ────────────────────────────────────────────────────────────────

// Implementation names a program, the client or the agent.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Label is the title, else the name.
func (i Implementation) Label() string {
	if i.Title != "" {
		return i.Title
	}
	return i.Name
}

// AuthMethod is a way to log in that the agent offers.
type AuthMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is "terminal" for a login the user runs in a terminal; empty
	// for one the agent handles itself.
	Type string `json:"type,omitempty"`
}

// InitializeResponse is the agent's half of the handshake.
type InitializeResponse struct {
	ProtocolVersion int             `json:"protocolVersion"`
	AgentInfo       *Implementation `json:"agentInfo,omitempty"`
	AuthMethods     []AuthMethod    `json:"authMethods,omitempty"`
}

// EnvVariable is one variable of an MCP server's environment.
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// McpServer is an MCP server the agent starts on stdio for the session.
// ACP requires args and env, even when empty.
type McpServer struct {
	Name    string        `json:"name"`
	Command string        `json:"command"`
	Args    []string      `json:"args"`
	Env     []EnvVariable `json:"env"`
}

// NewSessionRequest opens a conversation. Meta carries agent-specific
// options, which other agents ignore.
type NewSessionRequest struct {
	Cwd        string         `json:"cwd"`
	McpServers []McpServer    `json:"mcpServers"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

type newSessionResponse struct {
	SessionID string `json:"sessionId"`
}

// ContentBlock is a piece of a message. rimor sends and shows text; other
// kinds (images, resources) are shown by their type.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Text is a text block.
func Text(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

type promptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type promptResponse struct {
	StopReason string `json:"stopReason"`
}

// Stop reasons that end a prompt turn.
const (
	StopEndTurn   = "end_turn"
	StopMaxTokens = "max_tokens"
	StopMaxTurns  = "max_turn_requests"
	StopRefusal   = "refusal"
	StopCancelled = "cancelled"
)

// PlanEntry is one step of the agent's plan.
type PlanEntry struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending, in_progress, completed
}

// Update is a session/update: one of several kinds, named by Kind, that
// share this struct. Fields a kind does not use stay empty.
type Update struct {
	Kind string `json:"sessionUpdate"`

	// Message chunks (agent_message_chunk, agent_thought_chunk,
	// user_message_chunk) carry a ContentBlock here; tool calls carry an
	// array of ToolCallContent. See Chunk and ToolContent.
	Content json.RawMessage `json:"content,omitempty"`

	// tool_call and tool_call_update. An update sends only what changed.
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      *string         `json:"title,omitempty"` // also a notice's text
	Name       *string         `json:"name,omitempty"`
	ToolKind   *string         `json:"kind,omitempty"`
	Status     *string         `json:"status,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`

	// plan
	Entries []PlanEntry `json:"entries,omitempty"`

	// notice
	Severity string `json:"severity,omitempty"`
}

// Update kinds rimor shows. Others are ignored.
const (
	UpdateAgentMessage = "agent_message_chunk"
	UpdateAgentThought = "agent_thought_chunk"
	UpdateToolCall     = "tool_call"
	UpdateToolCallEdit = "tool_call_update"
	UpdatePlan         = "plan"
	UpdateNotice       = "notice"
)

// Chunk is a message chunk's content.
func (u Update) Chunk() ContentBlock {
	var b ContentBlock
	_ = json.Unmarshal(u.Content, &b)
	return b
}

// ToolContent is the text a tool call reports, from its content items.
func (u Update) ToolContent() string {
	var items []struct {
		Type    string       `json:"type"`
		Content ContentBlock `json:"content"`
		Path    string       `json:"path"`
	}
	if json.Unmarshal(u.Content, &items) != nil {
		return ""
	}
	var out string
	for _, it := range items {
		s := ""
		switch it.Type {
		case "content":
			s = it.Content.Text
		case "diff":
			s = "diff " + it.Path
		}
		if s != "" {
			if out != "" {
				out += "\n"
			}
			out += s
		}
	}
	return out
}

type sessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    Update `json:"update"`
}

// PermissionOption is a choice the agent offers for a tool call.
type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
}

// PermissionRequest asks whether a tool call may run.
type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  Update             `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// Handlers receive what the agent sends on its own.
type Handlers struct {
	// Update receives session/update notifications, in order.
	Update func(sessionID string, u Update)
	// Permission answers session/request_permission with the chosen
	// option's ID, or "" to cancel the tool call. It runs on its own
	// goroutine and may block until the user decides.
	Permission func(PermissionRequest) string
}

// ── Connection ──────────────────────────────────────────────────────────────

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type outgoing struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

// Conn is a JSON-RPC connection to an agent.
type Conn struct {
	h Handlers

	wmu sync.Mutex // one message at a time on the wire
	enc *json.Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan message
	done    chan struct{} // closed when the agent's output ends
	err     error         // why it ended
}

// NewConn talks to an agent that reads from w and writes to r. It reads
// until r ends; Done is closed then.
func NewConn(r io.Reader, w io.Writer, h Handlers) *Conn {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	c := &Conn{h: h, enc: enc, pending: map[int64]chan message{}, done: make(chan struct{})}
	go c.read(r)
	return c
}

// Done is closed when the agent's output ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, once Done is closed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) send(m outgoing) error {
	m.JSONRPC = "2.0"
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.enc.Encode(m)
}

func (c *Conn) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue // agents may log a stray line; nothing to answer
		}
		switch {
		case m.Method == "" && len(m.ID) > 0:
			c.deliver(m)
		case m.Method != "" && len(m.ID) > 0:
			go c.answer(m)
		case m.Method != "":
			c.notified(m)
		}
	}
	err := sc.Err()
	if err == nil {
		err = ErrClosed
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Conn) deliver(m message) {
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (c *Conn) notified(m message) {
	if m.Method != "session/update" || c.h.Update == nil {
		return
	}
	var n sessionNotification
	if json.Unmarshal(m.Params, &n) == nil {
		c.h.Update(n.SessionID, n.Update)
	}
}

// answer replies to a request from the agent. rimor advertises no file
// system and no terminal, so only permission requests are expected.
func (c *Conn) answer(m message) {
	reply := outgoing{ID: m.ID}
	switch m.Method {
	case "session/request_permission":
		var p PermissionRequest
		if err := json.Unmarshal(m.Params, &p); err != nil {
			reply.Error = &Error{Code: codeInvalidParams, Message: err.Error()}
			break
		}
		choice := ""
		if c.h.Permission != nil {
			choice = c.h.Permission(p)
		}
		if choice == "" {
			reply.Result = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		} else {
			reply.Result = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": choice}}
		}
	default:
		reply.Error = &Error{Code: codeNoMethod, Message: "rimor does not offer " + m.Method}
	}
	_ = c.send(reply)
}

// Call sends a request and decodes its result into out, which may be nil.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(outgoing{ID: id, Method: method, Params: params}); err != nil {
		c.forget(id)
		return fmt.Errorf("writing to the agent: %w", err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return c.Err()
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	}
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	if err := c.Err(); err != nil {
		return err
	}
	return c.send(outgoing{Method: method, Params: params})
}

// ── ACP methods ─────────────────────────────────────────────────────────────

// Initialize negotiates the protocol. rimor offers no file system and no
// terminal to the agent.
func (c *Conn) Initialize(ctx context.Context, client Implementation) (InitializeResponse, error) {
	var resp InitializeResponse
	err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": client,
	}, &resp)
	if err != nil {
		return resp, err
	}
	if resp.ProtocolVersion != ProtocolVersion {
		return resp, fmt.Errorf("the agent speaks ACP version %d; rimor speaks %d", resp.ProtocolVersion, ProtocolVersion)
	}
	return resp, nil
}

// Authenticate logs in with one of the agent's AuthMethods.
func (c *Conn) Authenticate(ctx context.Context, methodID string) error {
	return c.Call(ctx, "authenticate", map[string]string{"methodId": methodID}, nil)
}

// NewSession opens a conversation and returns its ID.
func (c *Conn) NewSession(ctx context.Context, req NewSessionRequest) (string, error) {
	if req.McpServers == nil {
		req.McpServers = []McpServer{}
	}
	var resp newSessionResponse
	if err := c.Call(ctx, "session/new", req, &resp); err != nil {
		return "", err
	}
	if resp.SessionID == "" {
		return "", errors.New("the agent opened no session")
	}
	return resp.SessionID, nil
}

// Prompt sends the user's message and waits for the turn to end; updates
// arrive through Handlers.Update meanwhile. It returns the stop reason.
func (c *Conn) Prompt(ctx context.Context, sessionID string, prompt []ContentBlock) (string, error) {
	var resp promptResponse
	err := c.Call(ctx, "session/prompt", promptRequest{SessionID: sessionID, Prompt: prompt}, &resp)
	return resp.StopReason, err
}

// Cancel asks the agent to end the current turn; Prompt then returns
// StopCancelled.
func (c *Conn) Cancel(sessionID string) error {
	return c.Notify("session/cancel", map[string]string{"sessionId": sessionID})
}
