//go:build windows

package hub

import "syscall"

// processName is unavailable on Windows: there is no /proc, and the toolhelp
// API would be the only way to read a process name. An empty result means
// the harness is never recognised, so the session records no pid and prune
// keeps it, which is the safe direction.
func processName(int) string { return "" }

// parentPID is unavailable on Windows for the same reason.
func parentPID(int) (int, error) { return 0, syscall.ENOSYS }

// harnessKeyOfProcess is unavailable on Windows: reading another process's
// environment needs DebugActiveProcess and a heap walk, which is not worth it
// for a repair that degrades to "not found" without it.
func harnessKeyOfProcess(int) string { return "" }

// liveHarnessKeys cannot answer on Windows, for the same reason. The false
// second result says so, and every caller then falls back to the pid check
// rather than reading an empty set as "no chat is running".
func liveHarnessKeys() (map[string]bool, bool) { return nil, false }

// processStartTime is unavailable on Windows: the creation time is readable
// through GetProcessTimes, but the two places it matters (telling a reused
// pid from the chat that used to hold it, and naming a session after its
// chat) already degrade to the safe answer without it. A zero means "unknown
// start", and a session whose start is unknown is never declared dead on the
// strength of the pid alone.
func processStartTime(int) int64 { return 0 }

// processAlive reports whether a pid still exists on Windows.
//
// os.FindProcess always succeeds there and Signal(0) is not supported, so the
// process handle is opened instead. OpenProcess failing with
// ERROR_INVALID_PARAMETER means the pid is gone; ERROR_ACCESS_DENIED means it
// exists but belongs to another user, which still counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	const (
		processQueryLimitedInformation = 0x1000
		errorInvalidParameter          = syscall.Errno(87)
	)

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")

	h, _, errno := openProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h != 0 {
		kernel32.NewProc("CloseHandle").Call(h)
		return true
	}
	return errno != errorInvalidParameter
}
