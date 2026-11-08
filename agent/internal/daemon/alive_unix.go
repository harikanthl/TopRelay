//go:build unix

package daemon

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process exists (EPERM: it exists but isn't ours).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
