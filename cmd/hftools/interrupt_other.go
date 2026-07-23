//go:build !linux

package main

// enableCbreak is a no-op on non-Linux platforms: single-key ESC detection needs
// platform-specific terminal handling, so there interruption relies on Ctrl+C
// (SIGINT) alone, which still cancels the operation.
func enableCbreak(fd int) (func(), bool) { return nil, false }
