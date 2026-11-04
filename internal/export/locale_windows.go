package export

import (
	"syscall"
	"unsafe"
)

// systemListSeparator is the user's list separator (Control Panel, Region),
// which Excel uses for CSV.
func systemListSeparator() rune { return localeInfo(0x0C) } // LOCALE_SLIST

// systemDecimalSeparator is the user's decimal separator.
func systemDecimalSeparator() rune { return localeInfo(0x0E) } // LOCALE_SDECIMAL

// localeInfo reads a one-character setting of the user's locale.
func localeInfo(what uintptr) rune {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetLocaleInfoEx")
	if proc.Find() != nil {
		return 0
	}
	buf := make([]uint16, 8)
	n, _, _ := proc.Call(0, what, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n < 2 { // the separator and the terminating zero
		return 0
	}
	s := syscall.UTF16ToString(buf[:n])
	if r := []rune(s); len(r) == 1 {
		return r[0]
	}
	return 0
}
