package main

import (
	"context"
	"errors"
	"os"
)

// errInterrupted is the user-facing error returned when a long operation is
// stopped with Ctrl+C or ESC.
var errInterrupted = errors.New("interrupted")

// watchInterrupt derives a cancellable context that is cancelled when the parent
// is (e.g. Ctrl+C via SIGINT) or when the user presses ESC or q on a terminal.
// On a terminal it puts stdin into cbreak mode for the duration — signals stay
// enabled, so Ctrl+C still works normally — and the returned stop func restores
// it. When stdin is not a terminal (piped, redirected, non-Linux), only the
// parent-cancellation path is active and stop just cancels.
func watchInterrupt(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	restore, ok := enableCbreak(int(os.Stdin.Fd()))
	if !ok {
		return ctx, cancel
	}
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			if n > 0 && (buf[0] == 0x1b || buf[0] == 'q' || buf[0] == 'Q') { // ESC or q
				cancel()
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return ctx, func() {
		restore()
		cancel()
	}
}
