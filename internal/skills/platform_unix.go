//go:build linux || darwin

package skills

import "syscall"

// Nonblocking opens prevent a swapped FIFO from hanging before fstat validation.
const readFlags = syscall.O_NONBLOCK | syscall.O_NOCTTY
const supportedPlatform = true
