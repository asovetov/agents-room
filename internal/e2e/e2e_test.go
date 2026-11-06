// Package e2e drives the real command-line binaries against one store, the
// way two agent sessions would.
//
// It builds the binaries once, then runs them as separate processes so the
// test exercises the actual process boundary, SQLite locking and exit codes.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agents-room-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	binDir = dir

	// On Windows, go build -o writes exactly the name it is given, so the
	// extension has to be part of it.
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for _, name := range []string{"agents-room", "agents-room-hook", "agents-room-mcp"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name+suffix), "./cmd/"+name)
		cmd.Dir = "../.."
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", name, err, out)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

type env struct {
	t    *testing.T
	db   string
	cwd  string
	room string // room name the hook derives from this directory
}

// scrubEnv removes harness messaging variables so a hook under test never
// posts into the developer's real session.
func scrubEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, "CLAUDE_CODE_MESSAGING_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	// The hook derives the room from the git top level, so the fixture must
	// be a repository for the room name to be predictable.
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	return &env{
		t:    t,
		db:   filepath.Join(dir, "room.db"),
		cwd:  dir,
		room: "repo:" + filepath.Base(dir),
	}
}

// run executes a binary with the shared store and returns stdout.
func (e *env) run(name string, stdin string, args ...string) (string, int) {
	e.t.Helper()
	exe := filepath.Join(binDir, name)
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(scrubEnv(os.Environ()), "AGENTS_ROOM_DB="+e.db)
	cmd.Dir = e.cwd
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		e.t.Fatalf("run %s: %v (stderr %s)", name, err, errOut.String())
	}
	return out.String(), code
}

func TestTwoProcessesExchangeAMessage(t *testing.T) {
	e := newEnv(t)

	aOut, code := e.run("agents-room", "", "join", "--room", e.room, "--key", "a", "--label", "session-a")
	if code != 0 {
		t.Fatalf("join a exit %d", code)
	}
	bOut, code := e.run("agents-room", "", "join", "--room", e.room, "--key", "b", "--label", "session-b")
	if code != 0 {
		t.Fatalf("join b exit %d", code)
	}
	a, b := strings.TrimSpace(aOut), strings.TrimSpace(bOut)

	if _, code := e.run("agents-room", "",
		"send", "--session", a, "--to", b, "--subject", "protocol", "--body", "dialect 42100"); code != 0 {
		t.Fatalf("send exit %d", code)
	}

	out, code := e.run("agents-room", "", "recv", "--session", b)
	if code != 0 {
		t.Fatalf("recv exit %d", code)
	}
	if !strings.Contains(out, "dialect 42100") {
		t.Fatalf("recv output = %q", out)
	}
}

func TestConcurrentProcessesClaimOneLease(t *testing.T) {
	e := newEnv(t)

	const n = 6
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		out, code := e.run("agents-room", "", "join", "--room", e.room,
			"--key", fmt.Sprintf("s%d", i))
		if code != 0 {
			t.Fatalf("join %d exit %d", i, code)
		}
		ids[i] = strings.TrimSpace(out)
	}

	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, code := e.run("agents-room", "",
				"lease", "claim", "--session", ids[i], "--path", "src/hot.go")
			results[i] = code
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, code := range results {
		if code == 0 {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d processes won the lease, want exactly 1", winners)
	}
}

func TestHookJoinsIdempotentlyAcrossSessionStarts(t *testing.T) {
	e := newEnv(t)
	payload := `{"session_id":"harness-session-1","cwd":"` + strings.ReplaceAll(e.cwd, `\`, `\\`) + `"}`

	// Claude Code fires SessionStart on startup, resume, clear, compact and
	// fork. All five are the same chat.
	for i := 0; i < 5; i++ {
		if _, code := e.run("agents-room-hook", payload, "--event=SessionStart", "--autonomy=2"); code != 0 {
			t.Fatalf("hook exit %d on start %d", code, i)
		}
	}

	out, code := e.run("agents-room", "", "who", "--room", e.room, "--json")
	if code != 0 {
		t.Fatalf("who exit %d", code)
	}
	var sessions []map[string]any
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("who output: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("five SessionStart events produced %d sessions, want 1", len(sessions))
	}
}

func TestHookStopReportsUnreadWithoutHolding(t *testing.T) {
	e := newEnv(t)
	payload := `{"session_id":"hs-1","cwd":"` + strings.ReplaceAll(e.cwd, `\`, `\\`) + `"}`

	// The hook joins at notify level, so Stop must stay silent.
	if _, code := e.run("agents-room-hook", payload, "--event=SessionStart", "--autonomy=0"); code != 0 {
		t.Fatalf("hook join exit %d", code)
	}

	out, _ := e.run("agents-room", "", "who", "--room", e.room, "--json")
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &sessions); err != nil || len(sessions) != 1 {
		t.Fatalf("who: %v (%s)", err, out)
	}

	// A second session sends to the room.
	other, _ := e.run("agents-room", "", "join", "--room", e.room, "--key", "other")
	if _, code := e.run("agents-room", "", "send",
		"--session", strings.TrimSpace(other), "--to", "*", "--body", "hello"); code != 0 {
		t.Fatalf("send exit %d", code)
	}

	stopOut, code := e.run("agents-room-hook", payload, "--event=Stop")
	if code != 0 {
		t.Fatalf("stop hook exit %d", code)
	}
	if strings.TrimSpace(stopOut) != "" {
		t.Fatalf("notify-level Stop produced output %q, want silence", stopOut)
	}

	// Autonomy is negotiated at join, so raising it means rejoining. The
	// hook does that on SessionStart with the new level.
	if _, code := e.run("agents-room-hook", payload, "--event=SessionStart", "--autonomy=1"); code != 0 {
		t.Fatalf("rejoin exit %d", code)
	}
	holdOut, _ := e.run("agents-room-hook", payload, "--event=Stop", "--autonomy=1")
	if !strings.Contains(holdOut, `"decision":"block"`) {
		t.Fatalf("hold-level Stop output = %q, want a block decision", holdOut)
	}
}

func TestHookSessionEndRemovesTheSession(t *testing.T) {
	e := newEnv(t)
	payload := `{"session_id":"hs-end","cwd":"` + strings.ReplaceAll(e.cwd, `\`, `\\`) + `"}`

	if _, code := e.run("agents-room-hook", payload, "--event=SessionStart"); code != 0 {
		t.Fatalf("join exit %d", code)
	}
	if _, code := e.run("agents-room-hook", payload, "--event=SessionEnd"); code != 0 {
		t.Fatalf("leave exit %d", code)
	}

	out, _ := e.run("agents-room", "", "who", "--room", e.room, "--json")
	var sessions []map[string]any
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("%d sessions remain after SessionEnd, want 0", len(sessions))
	}
}
