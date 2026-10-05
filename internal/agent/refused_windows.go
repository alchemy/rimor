package agent

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is WSAECONNREFUSED: nothing listens on a socket file.
const wsaeconnrefused = 10061

func isRefused(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == wsaeconnrefused
}
