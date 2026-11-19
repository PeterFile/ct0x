//go:build darwin || linux

package main

import (
	"os"
	"syscall"
)

func openBackgroundDispatchFile(path string) (*os.File, error) {
	// Refuse symlinks, and never block the scheduler opening a FIFO before fstat.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
