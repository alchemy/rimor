//go:build !unix

package paths

// private has nothing to check where the temporary directory is the
// user's own (Windows).
func private(string) error { return nil }
