//go:build !windows && !linux

package hub

import (
	"os"
	"syscall"
)

// This build covers macOS and the other Unix targets that have no /proc.
//
// Reading another process's name, start time and environment needs a syscall
// per field here (sysctl, libproc), and none of the three is available from
// the standard library alone. The answers degrade instead of lying:
//
//   - processName returns "", so HarnessPIDFromEnv never recognises a harness
//     and records no pid. A session with no pid is never pruned and reads as
//     `unknown`, which is the safe direction.
//   - processStartTime returns 0, so liveness falls back to "is the pid
//     running", the check that existed before start times were recorded.
//   - harnessKeyOfProcess returns "", so duplicate rows cannot be tied back
//     to their chat by key and doctor does not report them.
//   - liveHarnessKeys reports that it cannot answer, so liveness and prune
//     both fall back to the pid and still agree with each other.

// processName is unavailable without /proc.
func processName(int) string { return "" }

// parentPID is unavailable without /proc.
func parentPID(int) (int, error) { return 0, syscall.ENOSYS }

// harnessKeyOfProcess is unavailable without /proc.
func harnessKeyOfProcess(int) string { return "" }

// liveHarnessKeys cannot answer without /proc. The false second result is
// what keeps an empty set from reading as "no chat is running".
func liveHarnessKeys() (map[string]bool, bool) { return nil, false }

// processStartTime is unavailable without /proc.
func processStartTime(int) int64 { return 0 }

// processAlive reports whether a pid still exists.
//
// Signal 0 performs the permission and existence checks without delivering
// anything: nil means alive, EPERM means alive but owned by another user, and
// ESRCH means gone. A pid of zero or less was never recorded and is treated
// as unknown, not dead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	switch err {
	case nil, syscall.EPERM:
		return true
	case syscall.ESRCH:
		return false
	default:
		// An unexpected error is not evidence of death.
		return true
	}
}

// hasProcFS reports that this platform cannot answer the process questions
// the tests exercise. It is false here and true on Linux, so a test can skip
// instead of asserting a fallback it was not written for.
const hasProcFS = false
