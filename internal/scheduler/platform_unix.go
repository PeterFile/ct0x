//go:build linux || darwin

package scheduler

import (
	"fmt"
	"os"
	"syscall"
)

func workspaceIdentity(info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrInvalidWorkspace
	}
	return fmt.Sprintf("workspace:%d:%d", stat.Dev, stat.Ino), nil
}
