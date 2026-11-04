//go:build unix

package acp

import (
	"os/exec"
	"syscall"
)

// ownGroup starts the agent in a process group of its own, so that
// killGroup also ends what it started: adapters run through npx start
// node, which starts the agent's own binary.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
