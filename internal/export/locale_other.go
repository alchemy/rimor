//go:build !windows

package export

// systemListSeparator has no system setting to read outside Windows: the
// locale environment variables tell.
func systemListSeparator() rune { return 0 }

func systemDecimalSeparator() rune { return 0 }
