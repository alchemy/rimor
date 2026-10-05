package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// mcp runs the MCP server over the given messages and returns its
// replies, keyed by id.
func mcp(t *testing.T, call caller, msgs ...string) map[string]map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(strings.Join(msgs, "\n")+"\n"), &out, "v9.9.9", call); err != nil {
		t.Fatal(err)
	}
	replies := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("reply %q: %v", line, err)
		}
		if m["jsonrpc"] != "2.0" {
			t.Errorf("reply without jsonrpc 2.0: %s", line)
		}
		id, _ := json.Marshal(m["id"])
		replies[string(id)] = m
	}
	return replies
}

func TestMCPHandshakeAndTools(t *testing.T) {
	replies := mcp(t, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":"p","method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
		`not json`,
	)
	if len(replies) != 5 { // the notification gets none
		t.Errorf("%d replies: %v", len(replies), replies)
	}

	init := replies["1"]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" {
		t.Errorf("did not accept the client's version: %v", init["protocolVersion"])
	}
	if info := init["serverInfo"].(map[string]any); info["name"] != "rimor" || info["version"] != "v9.9.9" {
		t.Errorf("serverInfo %v", info)
	}
	if !strings.Contains(init["instructions"].(string), "cannot execute") {
		t.Error("instructions do not say the agent cannot execute SQL")
	}

	tools := replies["2"]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range tools {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s: input schema is not an object", tm["name"])
		}
	}
	if got := strings.Join(names, " "); got != "list_connections list_objects search_objects describe open_query update_query list_tabs read_tab wait_for_run read_results" {
		t.Errorf("tools: %s", got)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "run") || strings.HasPrefix(name, "exec") || strings.Contains(name, "execute") {
			t.Errorf("a tool that sounds like it runs SQL: %s", name)
		}
	}

	if _, ok := replies[`"p"`]["result"]; !ok {
		t.Error("no ping reply")
	}
	if e := replies["3"]["error"].(map[string]any); e["code"].(float64) != errNoMethod {
		t.Errorf("unknown method: %v", e)
	}
	if e := replies["null"]["error"].(map[string]any); e["code"].(float64) != errParse {
		t.Errorf("parse error: %v", e)
	}
}

func TestMCPUnknownProtocolVersion(t *testing.T) {
	replies := mcp(t, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := replies["1"]["result"].(map[string]any)["protocolVersion"]; v != protocolVersions[0] {
		t.Errorf("offered %v", v)
	}
}

func TestMCPToolCalls(t *testing.T) {
	var gotTool, gotArgs string
	call := func(_ context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
		gotTool, gotArgs = tool, string(args)
		if tool == "describe" {
			return nil, errors.New("no such table or view")
		}
		return json.RawMessage(`{"tab":7}`), nil
	}
	replies := mcp(t, call,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"open_query","arguments":{"connection":"ERP","sql":"SELECT 1"}}}`,
	)
	res := replies["1"]["result"].(map[string]any)
	if res["isError"] != false || res["content"].([]any)[0].(map[string]any)["text"] != `{"tab":7}` {
		t.Errorf("result %v", res)
	}
	if gotTool != "open_query" || gotArgs != `{"connection":"ERP","sql":"SELECT 1"}` {
		t.Errorf("forwarded %s %s", gotTool, gotArgs)
	}

	// Failures are results the agent sees, not protocol errors.
	replies = mcp(t, call, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"describe","arguments":{}}}`)
	res = replies["2"]["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "no such table") {
		t.Errorf("error result %v", res)
	}

	// Tools that do not exist are protocol errors, never forwarded.
	gotTool = ""
	replies = mcp(t, call, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"run_query","arguments":{}}}`)
	if _, ok := replies["3"]["error"]; !ok || gotTool != "" {
		t.Errorf("unknown tool: %v, forwarded %q", replies["3"], gotTool)
	}
}

// runtimeDir points the runtime directory at a fresh, short path: socket
// paths are limited to about 100 bytes.
func runtimeDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	return dir
}

func TestSocketRoundTrip(t *testing.T) {
	runtimeDir(t)
	ctx := context.Background()
	if _, err := Call(ctx, "list_tabs", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("with nothing running: %v", err)
	}

	srv, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(func(_ context.Context, tool string, args json.RawMessage) (any, error) {
		if tool == "fail" {
			return nil, errors.New("refused")
		}
		return map[string]any{"tool": tool, "args": args}, nil
	})

	res, err := Call(ctx, "list_tabs", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"args":{"x":1},"tool":"list_tabs"}` {
		t.Errorf("result %s", res)
	}
	if _, err := Call(ctx, "fail", nil); err == nil || err.Error() != "refused" {
		t.Errorf("error %v", err)
	}

	srv.Close()
	if _, err := os.Stat(srv.Path()); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
	if _, err := Call(ctx, "list_tabs", nil); !errors.Is(err, ErrNotRunning) {
		t.Errorf("after close: %v", err)
	}
}
