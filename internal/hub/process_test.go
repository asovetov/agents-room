package hub

import (
	"os/exec"
	"runtime"
	"testing"
)

// deadPID returns a pid that is certain to belong to no running process.
//
// A test cannot just guess a number: pid reuse would make the result flaky,
// and a hard-coded high pid differs across platforms. Starting a process and
// letting it exit gives a pid the OS has just retired.
func deadPID(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("starting and reaping a process for a retired pid is not reliable here")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	return cmd.Process.Pid
}

func TestProcessAliveSeesThisProcess(t *testing.T) {
	if !processAlive(1) && runtime.GOOS != "windows" {
		t.Skip("pid 1 is not visible in this environment")
	}
	if !processAlive(0) {
		t.Fatal("an unknown pid was reported dead; pruning would guess")
	}
}

func TestProcessAliveRejectsRetiredPID(t *testing.T) {
	if processAlive(deadPID(t)) {
		t.Fatal("a process that has exited was reported alive")
	}
}
