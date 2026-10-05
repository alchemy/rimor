//go:build unix

package agent

import (
	"errors"
	"syscall"
)

// isRefused reports that nothing listens on a socket file.
func isRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
