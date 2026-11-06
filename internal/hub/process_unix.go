//go:build !windows

package hub

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processName returns the executable name of a pid, or "" when it is gone.
func processName(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parentPID returns the pid that started pid, read from /proc.
func parentPID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The format is "pid (comm) state ppid ...", and comm may contain
	// spaces and parentheses, so the fields are counted from the last ')'.
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, fmt.Errorf("parse stat for %d: no command field", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("parse stat for %d: too few fields", pid)
	}
	return strconv.Atoi(fields[1])
}

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
