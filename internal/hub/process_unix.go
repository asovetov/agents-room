//go:build linux

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

// harnessKeyOfProcess returns the harness session id a process inherited, or
// "" when the process is gone, is not ours to read, or carries none.
//
// It is how a row minted without a harness key is tied back to the chat it
// belongs to. The row of such a session records the pid of the process that
// joined -- the MCP server, when the join came through room_join -- and that
// process has the chat's own session id in its environment. Reading it from
// the process is legitimate: the process is this user's, and the value is the
// harness's, not a secret.
func harnessKeyOfProcess(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	for _, entry := range strings.Split(string(data), "\x00") {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID":
			if v := strings.TrimSpace(value); v != "" {
				return v
			}
		}
	}
	return ""
}

// liveHarnessKeys returns the harness session ids carried by the processes
// running right now, and whether the platform can answer at all.
//
// This is the honest liveness test, better than a pid in both directions. A
// chat's MCP server is restarted under it, and every restart changes the pid
// the row recorded while the chat itself carries on. The key does not change:
// it is what the harness rejoins with. A key that no process holds belongs to
// a chat that is gone, however recently the row was touched.
//
// The scan is one pass over /proc, which is why it is done once per call and
// not per session.
func liveHarnessKeys() (map[string]bool, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	keys := make(map[string]bool, 8)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// A process this user cannot read is skipped, not counted as absent:
		// it is not evidence about any chat of ours.
		if key := harnessKeyOfProcess(pid); key != "" {
			keys[key] = true
		}
	}
	return keys, true
}

// processStartTime returns the boot-relative start time of a pid, in clock
// ticks since boot, or 0 when it cannot be read.
//
// It is what tells a pid from the process that used to hold it. A session row
// records the pid of its chat; when that chat exits the number is free, and
// the next process the OS hands it to is not the same chat. Comparing the
// start time catches that where a bare "is it alive" check cannot.
//
// The value is only ever compared with another value read the same way, so
// its unit does not matter to callers.
func processStartTime(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0
	}
	// After comm: state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice threads
	// itrealvalue starttime -- the 20th field, so index 19 here.
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return 0
	}
	v, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
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

// hasProcFS reports that this platform can answer the process questions the
// tests exercise.
const hasProcFS = true
