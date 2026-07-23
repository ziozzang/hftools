//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// enableCbreak switches the terminal at fd into cbreak mode: each keystroke is
// delivered immediately and not echoed, so a lone ESC or q can be read, while
// ISIG stays on (Ctrl+C keeps raising SIGINT) and OPOST stays on (printed
// newlines still render). macOS uses TIOCGETA/TIOCSETA where Linux uses
// TCGETS/TCSETS. It returns a restore func and true on success, or false when fd
// is not a terminal.
func enableCbreak(fd int) (func(), bool) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, false
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, false
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&old)))
	}, true
}
