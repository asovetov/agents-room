package hub

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
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
//
// The test cannot rely on a harness being somewhere up its own process chain:
// under CI there is none, and the answer is then 0 by design. A stand-in
// process that outlives the join is started instead, and the harness is made
// to name it the way a real one would.
func TestJoinRecordsHarnessPIDNotTheJoiningProcess(t *testing.T) {
	helper := exec.Command(os.Args[0], "-test.run=TestHarnessPIDHelperProcess")
	helper.Env = append(os.Environ(), "AGENTS_ROOM_HELPER=1")
	if err := helper.Start(); err != nil {
		t.Skipf("cannot start a stand-in harness: %v", err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})
	t.Setenv("AGENTS_ROOM_PID", strconv.Itoa(helper.Process.Pid))

	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "k"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.PID == os.Getpid() {
		t.Fatalf("join recorded its own pid %d, which dies with the command", s.PID)
	}
	if s.PID != helper.Process.Pid {
		t.Fatalf("join recorded pid %d, want the harness pid %d", s.PID, helper.Process.Pid)
	}
}

// TestHarnessPIDHelperProcess is not a test. It is the process that stands in
// for the chat: it has to be alive while the test runs, and it exits on its
// own if the test never reaps it.
func TestHarnessPIDHelperProcess(t *testing.T) {
	if os.Getenv("AGENTS_ROOM_HELPER") != "1" {
		t.Skip("only runs as the stand-in harness")
	}
	time.Sleep(30 * time.Second)
}

// The recorded pid must be a process that outlives the command. Walking to
// the nearest parent is not enough: a hook runs under a shell that exits
// immediately, so the chain is followed until a known harness appears.
func TestHarnessPIDIsNotTheImmediateParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process names are read from /proc on this path")
	}
	pid := HarnessPIDFromEnv()
	if pid == os.Getppid() && !harnessProcessNames[processName(pid)] {
		t.Fatalf("recorded the immediate parent %d (%q), which is not a harness",
			pid, processName(pid))
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

// A pid is a number the OS reuses. A row records the start time of the
// process it named, so a session whose chat exited is reported gone even
// when something else is running at that number now.
//
// The old check asked only "is this pid alive", which answers about the
// stranger that inherited the number and calls a dead chat present.
func TestLivenessRejectsReusedPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("start times are read from /proc on this path")
	}
	self := os.Getpid()
	real := processStartTime(self)
	if real == 0 {
		t.Skip("this platform does not report a start time")
	}

	if got := livenessOf(self, real); got != LivenessAlive {
		t.Fatalf("liveness of this process = %q, want %q", got, LivenessAlive)
	}
	// Same pid, a start time from another process: the number is in use, but
	// not by the process the row was written for.
	if got := livenessOf(self, real+1); got != LivenessGone {
		t.Fatalf("liveness of a reused pid = %q, want %q", got, LivenessGone)
	}
}

// A row with no pid at all is unknown, not gone: the join could not find a
// harness process, and deleting on that would take a live chat's leases.
func TestLivenessOfUnknownPIDIsUnknown(t *testing.T) {
	if got := livenessOf(0, 0); got != LivenessUnknown {
		t.Fatalf("liveness of pid 0 = %q, want %q", got, LivenessUnknown)
	}
}

// A row written before start times were recorded has 0 there. The pid alone
// then has to do, which is the behaviour it had before the column existed.
func TestLivenessWithoutStartTimeFallsBackToThePID(t *testing.T) {
	if got := livenessOf(os.Getpid(), 0); got != LivenessAlive {
		t.Fatalf("liveness with no start time = %q, want %q", got, LivenessAlive)
	}
	if got := livenessOf(deadPID(t), 0); got != LivenessGone {
		t.Fatalf("liveness of a retired pid = %q, want %q", got, LivenessGone)
	}
}

// The join has to record the start time, or the row cannot tell its own
// process from a later one that inherits the number.
func TestJoinRecordsTheStartTimeOfThePID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("start times are read from /proc on this path")
	}
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", PID: os.Getpid()})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.PIDStartedAt == 0 {
		t.Fatal("join recorded no start time, so a reused pid would read as this chat")
	}
	if want := processStartTime(os.Getpid()); s.PIDStartedAt != want {
		t.Fatalf("start time = %d, want %d", s.PIDStartedAt, want)
	}
}

// standInChat starts a process that looks like a harness chat carrying key,
// and returns its pid. The process must be started with the key in its
// environment, not have it set afterwards: /proc/<pid>/environ is a snapshot
// taken when the process starts.
func standInChat(t *testing.T, key string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHarnessPIDHelperProcess")
	cmd.Env = append(os.Environ(),
		"AGENTS_ROOM_HELPER=1",
		"CLAUDE_CODE_SESSION_ID="+key,
	)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a stand-in chat: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// Touch refreshes the pid, because a tool call proves which process the chat
// is running as now. It must only do that for a row this process owns: an
// operator touching another session must not attach its own pid to it.
func TestTouchRefreshesPIDOnlyForItsOwnSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process start times are read from /proc on this path")
	}
	ctx := context.Background()
	h := newHub(t)

	// The chat this test stands in for, and the key it rejoins with.
	chat := standInChat(t, "my-key")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "my-key")
	t.Setenv("AGENTS_ROOM_PID", strconv.Itoa(chat))

	own, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "my-key", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "someone-else", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Touch(ctx, own.ID); err != nil {
		t.Fatalf("touch own: %v", err)
	}
	if err := h.Touch(ctx, other.ID); err != nil {
		t.Fatalf("touch other: %v", err)
	}

	var ownPID, otherPID int
	var ownStarted int64
	_ = h.db.SQL().QueryRowContext(ctx, `SELECT pid, pid_started_at FROM sessions WHERE id = ?`, own.ID).Scan(&ownPID, &ownStarted)
	_ = h.db.SQL().QueryRowContext(ctx, `SELECT pid FROM sessions WHERE id = ?`, other.ID).Scan(&otherPID)

	// A restarted MCP server under the same chat is exactly this: the row's
	// recorded pid is stale, and the tool call supplies the live one.
	if ownPID != chat {
		t.Fatalf("own session pid = %d, want the chat's %d", ownPID, chat)
	}
	if ownStarted == 0 {
		t.Fatal("the refreshed pid was recorded without its start time")
	}
	if otherPID != 4242 {
		t.Fatalf("touching another session rewrote its pid to %d", otherPID)
	}
}
