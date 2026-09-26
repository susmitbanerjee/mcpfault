//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on ANSI escape handling for the console (needed on older Windows consoles).
func enableVT() bool {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
