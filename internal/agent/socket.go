// Package agent lets an AI agent work with the rimor the user has open.
//
// The agent starts "rimor mcp", an MCP server on stdin and stdout (see
// RunMCP). That process holds no connections and no credentials: it
// passes each tool call over a local socket to the running rimor, which
// answers from its explorer and query tabs. Nothing an agent sends is
// executed; it can only put SQL in a tab for the user to run.
//
// The socket lives in the user's runtime directory (see paths.Runtime),
// one per running rimor, named after its process id. Each connection
// carries one request and its response, as JSON lines.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"rimor.dev/internal/paths"
)

// Request is a tool call from rimor mcp.
type Request struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response is a tool's answer: a result, or an error for the agent.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Handler answers a tool call. Its error is shown to the agent.
type Handler func(ctx context.Context, tool string, args json.RawMessage) (any, error)

// requestTimeout bounds one call: catalog queries run under their own,
// shorter timeouts.
const requestTimeout = 90 * time.Second

// Server is the running rimor's end of the socket.
type Server struct {
	ln   net.Listener
	path string
}

// Listen opens this process's socket in the runtime directory, removing
// those left behind by rimor processes that are gone.
func Listen() (*Server, error) {
	dir, err := paths.MakeRuntime()
	if err != nil {
		return nil, err
	}
	removeStale(dir)
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".sock")
	_ = os.Remove(path) // a previous process with the same id
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600) // the directory is private already
	return &Server{ln: ln, path: path}, nil
}

// Path is the socket's file.
func (s *Server) Path() string { return s.path }

// Serve answers requests until Close.
func (s *Server) Serve(h Handler) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go serveConn(conn, h)
	}
}

// Close stops serving and removes the socket.
func (s *Server) Close() error {
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

func serveConn(conn net.Conn, h Handler) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout + 5*time.Second))

	var resp Response
	var req Request
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	if err != nil {
		resp.Error = "bad request: " + err.Error()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		result, err := h(ctx, req.Tool, req.Args)
		cancel()
		if err != nil {
			resp.Error = err.Error()
		} else if resp.Result, err = marshal(result); err != nil {
			resp.Error = err.Error()
		}
	}
	out, _ := marshal(resp)
	_, _ = conn.Write(append(out, '\n'))
}

// marshal writes JSON as an agent reads it best: SQL keeps its < and >
// rather than \u003c escapes meant for HTML.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// ErrNotRunning means no rimor is open to take the call.
var ErrNotRunning = errors.New("rimor is not running: ask the user to start rimor in a terminal, then try again")

// Call sends one request to the most recently started rimor.
func Call(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}

	req, err := json.Marshal(Request{Tool: tool, Args: args})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("rimor did not answer: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}

// dial connects to the newest socket that answers.
func dial(ctx context.Context) (net.Conn, error) {
	dir, err := paths.Runtime()
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	for _, path := range sockets(dir) {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := d.DialContext(dctx, "unix", path)
		cancel()
		if err == nil {
			return conn, nil
		}
	}
	return nil, ErrNotRunning
}

// sockets lists the sockets in dir, newest first.
func sockets(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type sock struct {
		path string
		mod  time.Time
	}
	var socks []sock
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		socks = append(socks, sock{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.Slice(socks, func(i, j int) bool { return socks[i].mod.After(socks[j].mod) })
	out := make([]string, len(socks))
	for i, s := range socks {
		out[i] = s.path
	}
	return out
}

// removeStale deletes sockets nobody listens on: a rimor that crashed
// leaves its socket behind.
func removeStale(dir string) {
	for _, path := range sockets(dir) {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			continue
		}
		if isRefused(err) {
			_ = os.Remove(path)
		}
	}
}
