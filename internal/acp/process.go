package acp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNotInstalled means the agent's command is not in PATH.
var ErrNotInstalled = errors.New("not installed (or not in PATH)")

// Agent is an agent running as a child process.
type Agent struct {
	*Conn
	cmd    *exec.Cmd
	stdin  interface{ Close() error }
	stderr *tail
	exited chan struct{}
}

// Start runs an agent: command and its arguments, with env added to
// rimor's environment, in dir.
func Start(command []string, env map[string]string, dir string, h Handlers) (*Agent, error) {
	if len(command) == 0 {
		return nil, errors.New("no command for the agent")
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command[0], ErrNotInstalled)
	}
	cmd := exec.Command(path, command[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	ownGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	a := &Agent{cmd: cmd, stdin: stdin, stderr: &tail{max: 4 << 10}, exited: make(chan struct{})}
	cmd.Stderr = a.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a.Conn = NewConn(stdout, stdin, h)
	go func() {
		_ = cmd.Wait()
		close(a.exited)
	}()
	return a, nil
}

// Stderr is the end of what the agent wrote on stderr, for an error
// message when it fails.
func (a *Agent) Stderr() string { return a.stderr.String() }

// Close ends the agent: closing its input asks it to exit, and after a
// grace period it is killed, with the processes it started.
func (a *Agent) Close() {
	_ = a.stdin.Close()
	select {
	case <-a.exited:
	case <-time.After(2 * time.Second):
		killGroup(a.cmd)
		<-a.exited
	}
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = t.b[over:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}
