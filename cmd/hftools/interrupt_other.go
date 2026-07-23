//go:build !linux && !darwin

package main

// enableCbreak is a no-op on platforms without a termios implementation here
// (e.g. Windows): single-key ESC detection needs platform-specific terminal
// handling, so there interruption relies on Ctrl+C (SIGINT) alone, which still
// cancels the operation.
func enableCbreak(fd int) (func(), bool) { return nil, false }
