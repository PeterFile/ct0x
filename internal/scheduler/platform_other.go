//go:build !linux && !darwin

package scheduler

import "os"

// Fail closed until directory identity locking is supported on this platform.
func workspaceIdentity(os.FileInfo) (string, error) {
	return "", ErrWorkspaceUnsupported
}
