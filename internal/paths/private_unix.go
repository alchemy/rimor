//go:build unix

package paths

import (
	"fmt"
	"os"
	"syscall"
)

// private checks that dir is a directory, not a link, that only the user
// can enter: in a shared temporary directory another user could have made
// it first.
func private(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a directory of yours", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
