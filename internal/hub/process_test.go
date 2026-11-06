package hub

import (
	"context"
	"os"
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

// The joining process exits within milliseconds of the join, so its own pid
// is the wrong thing to record: prune would see a live chat as dead.
func TestJoinRecordsHarnessPIDNotTheJoiningProcess(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "k"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.PID == os.Getpid() {
		t.Fatalf("join recorded its own pid %d, which dies with the command", s.PID)
	}
	if s.PID <= 0 {
		t.Fatalf("join recorded pid %d, so liveness cannot be checked at all", s.PID)
	}
}

// An explicit pid still wins: a harness that knows better must be able to
// say which process owns the chat.
func TestJoinKeepsExplicitPID(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", PID: 4242})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.PID != 4242 {
		t.Fatalf("pid = %d, want the explicit 4242", s.PID)
	}
}
