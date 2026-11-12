package wake

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// recordWake replaces the spawn with one that records its arguments and does
// not start an agent.
func recordWake(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := runWake
	runWake = func(bin, key, line string) *exec.Cmd {
		got = append(got, bin, key, line)
		// A command that succeeds without doing anything: the same code path
		// as a real spawn, minus the agent turn.
		return exec.Command("true")
	}
	t.Cleanup(func() { runWake = old })
	return &got
}

func TestHermesCLIRunsResumeWithTheNudge(t *testing.T) {
	got := recordWake(t)
	t.Setenv("HERMES_BIN", "/usr/local/bin/hermes")

	if err := HermesCLI("local-abc", "[agents-room] 1 unread; call msg_recv"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(*got) != 3 {
		t.Fatalf("spawned %v, want bin, key and line", *got)
	}
	if (*got)[0] != "/usr/local/bin/hermes" {
		t.Fatalf("bin = %q", (*got)[0])
	}
	if (*got)[1] != "local-abc" {
		t.Fatalf("key = %q, want the harness key", (*got)[1])
	}
	if !strings.Contains((*got)[2], "msg_recv") {
		t.Fatalf("line = %q, want the nudge", (*got)[2])
	}
}

// No key means no turn: resuming nothing would start a fresh session, which
// is a different chat and would never read the message.
func TestHermesCLIWithoutAKeyDoesNotSpawn(t *testing.T) {
	got := recordWake(t)
	t.Setenv("HERMES_BIN", "/usr/local/bin/hermes")

	if err := HermesCLI("", "line"); err == nil {
		t.Fatal("wake without a key succeeded")
	}
	if len(*got) != 0 {
		t.Fatalf("spawned %v for an empty key", *got)
	}
}

// A missing binary is reported rather than pretended away.
func TestHermesCLIMissingBinaryIsAnError(t *testing.T) {
	got := recordWake(t)
	t.Setenv("HERMES_BIN", "")
	t.Setenv("PATH", t.TempDir())

	if err := HermesCLI("local-abc", "line"); err == nil {
		t.Fatal("wake without a hermes binary succeeded")
	}
	if len(*got) != 0 {
		t.Fatalf("spawned %v without a binary", *got)
	}
}

// The backend is off unless the operator wrote it down.
func TestEnabledIsOffByDefault(t *testing.T) {
	t.Setenv("AGENTS_ROOM_WAKE", filepath.Join(t.TempDir(), "absent"))
	if Enabled() {
		t.Fatal("wake is on with no setting file")
	}
}

func TestEnabledReadsTheOperatorsChoice(t *testing.T) {
	for _, c := range []struct {
		content string
		want    bool
	}{
		{"hermes=cli\n", true},
		{"hermes\n", true},
		{"# off for now\n", false},
		{"hermes=cli  # turned on deliberately\n", true},
		{"", false},
	} {
		path := filepath.Join(t.TempDir(), "wake")
		if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENTS_ROOM_WAKE", path)
		if got := Enabled(); got != c.want {
			t.Errorf("Enabled() with %q = %v, want %v", c.content, got, c.want)
		}
	}
}

// A burst of messages is one turn, not one per message.
func TestAllowWakeIsSingleFlightPerSession(t *testing.T) {
	resetSingleFlight()
	t.Cleanup(resetSingleFlight)
	if !AllowWake("s1") {
		t.Fatal("first wake was refused")
	}
	if AllowWake("s1") {
		t.Fatal("a second wake inside the window was allowed")
	}
	// Another session is unaffected.
	if !AllowWake("s2") {
		t.Fatal("one session's window silenced another")
	}
}

// The environment the turn is started with carries the session id, so its
// hooks address the session that was woken rather than a new one.
func TestHermesWakeEnvCarriesTheSessionID(t *testing.T) {
	var captured *exec.Cmd
	old := runWake
	runWake = func(bin, key, line string) *exec.Cmd {
		captured = exec.Command("true")
		return captured
	}
	t.Cleanup(func() { runWake = old })
	t.Setenv("HERMES_BIN", "/usr/local/bin/hermes")

	if err := HermesCLI("local-abc", "line"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	want := "HERMES_SESSION_ID=local-abc"
	for _, e := range captured.Env {
		if e == want {
			return
		}
	}
	t.Fatalf("the woken turn's environment lacks %q; its hooks would register a new session", want)
}
