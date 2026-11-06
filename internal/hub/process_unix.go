//go:build !windows

package hub

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether a pid still exists.
//
// Signal 0 performs the permission and existence checks without delivering
// anything. Three answers are possible: nil means alive, EPERM means alive
// but owned by another user, and ESRCH means gone. A pid this process started
// and reaped answers os.ErrProcessDone instead of ESRCH, so both count as
// gone. A pid of zero or less was never recorded and is treated as unknown,
// not dead: pruning on a guess would delete a live session's leases.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return false
	default:
		// An unexpected error is not evidence of death.
		return true
	}
}
