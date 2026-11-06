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
