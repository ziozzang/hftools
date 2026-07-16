//go:build !unix

package main

// freeBytes is unavailable on this platform.
func freeBytes(path string) (int64, bool) { return 0, false }
