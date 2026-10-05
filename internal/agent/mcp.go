package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
)

// The MCP side: JSON-RPC 2.0 over stdin and stdout, one message per line,
// as the stdio transport defines it. rimor mcp serves tools only.

// protocolVersions are the MCP revisions this server speaks, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	errParse         = -32700
	errNoMethod      = -32601
	errInvalidParams = -32602
)

// caller sends a tool call to rimor; Call, except in tests.
type caller func(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error)

// RunMCP serves MCP on in and out until in ends.
func RunMCP(in io.Reader, out io.Writer, version string) error {
	return serveMCP(in, out, version, Call)
}

func serveMCP(in io.Reader, out io.Writer, version string, call caller) error {
	var mu sync.Mutex
	enc := json.NewEncoder(out) // one message per line, no embedded newlines
	enc.SetEscapeHTML(false)
	send := func(m rpcMessage) {
		m.JSONRPC = "2.0"
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(m)
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m rpcMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			send(rpcMessage{ID: json.RawMessage("null"), Error: &rpcError{errParse, err.Error()}})
			continue
		}
		if m.Method == "" {
			continue // a response; this server sends no requests
		}
		if len(m.ID) == 0 {
			continue // a notification: initialized, cancelled, …
		}
		if m.Method == "tools/call" {
			// Calls may take a while (a catalog query); keep reading.
			wg.Add(1)
			go func() {
				defer wg.Done()
				send(callTool(m, call))
			}()
			continue
		}
		send(handle(m, version))
	}
	return sc.Err()
}

func handle(m rpcMessage, version string) rpcMessage {
	reply := rpcMessage{ID: m.ID}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		reply.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "rimor", "title": "rimor", "version": version},
			"instructions":    Instructions,
		}
	case "ping":
		reply.Result = map[string]any{}
	case "tools/list":
		reply.Result = map[string]any{"tools": Tools}
	default:
		reply.Error = &rpcError{errNoMethod, "method not found: " + m.Method}
	}
	return reply
}

func callTool(m rpcMessage, call caller) rpcMessage {
	reply := rpcMessage{ID: m.ID}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil || p.Name == "" {
		reply.Error = &rpcError{errInvalidParams, "tools/call needs a tool name"}
		return reply
	}
	if !slices.ContainsFunc(Tools, func(t Tool) bool { return t.Name == p.Name }) {
		reply.Error = &rpcError{errInvalidParams, "unknown tool: " + p.Name}
		return reply
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	result, err := call(ctx, p.Name, p.Arguments)
	// Tool failures go to the agent as results, so it can recover.
	if err != nil {
		reply.Result = toolResult(err.Error(), true)
		return reply
	}
	reply.Result = toolResult(string(result), false)
	return reply
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}
