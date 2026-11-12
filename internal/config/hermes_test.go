package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A config with foreign hooks and comments, shaped like a real one: the
// operator's file must survive the edit.
const hermesFixture = `# Hermes config, hand-edited
model:
  provider: custom
  default: some/model

hooks:
  pre_tool_call:
    - command: /usr/local/bin/other-tool hook
      timeout: 20
  on_session_start:
    - command: /usr/local/bin/other-tool start
      timeout: 20

# ── Security ──────────────────────────────────────────
# Comments matter and must not be reformatted away.
gateway:
  multiplex_profiles: true
`

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The plan wires every event and the MCP server, and says what it added.
func TestPlanHermesWiresEverything(t *testing.T) {
	path := writeFixture(t, hermesFixture)
	plan, err := PlanHermes(path)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Complete() {
		t.Fatal("plan says a bare config is already wired up")
	}
	for _, want := range []string{
		"--harness=hermes --event=pre_verify",
		"--harness=hermes --event=pre_tool_call",
		"--harness=hermes --event=on_session_start",
		"--harness=hermes --event=pre_llm_call",
		// The end-of-turn event must be Stop: on_session_end maps to "leave".
		"--harness=hermes --event=Stop",
		"--harness=hermes --event=on_session_finalize",
		"hooks_auto_accept: true",
		"agents-room:",
		"args: [mcp]",
	} {
		if !strings.Contains(plan.Updated, want) {
			t.Errorf("updated config lacks %q\n---\n%s", want, plan.Updated)
		}
	}
}

// The operator's own hooks and comments survive.
func TestPlanHermesKeepsForeignContent(t *testing.T) {
	path := writeFixture(t, hermesFixture)
	plan, err := PlanHermes(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/usr/local/bin/other-tool hook",
		"/usr/local/bin/other-tool start",
		"# Comments matter and must not be reformatted away.",
		"# ── Security ──",
		"multiplex_profiles: true",
	} {
		if !strings.Contains(plan.Updated, want) {
			t.Errorf("splice dropped %q\n---\n%s", want, plan.Updated)
		}
	}
}

// Running the plan twice changes nothing the second time, which is what makes
// `doctor --fix` safe to repeat.
func TestPlanHermesIsIdempotent(t *testing.T) {
	path := writeFixture(t, hermesFixture)
	first, err := PlanHermes(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyHermesPlan(first, time.Now()); err != nil {
		t.Fatal(err)
	}

	second, err := PlanHermes(path)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Complete() {
		t.Fatalf("a wired config still reports work to do: %v", second.Missing)
	}
	if second.Updated != first.Updated {
		t.Fatal("a second plan would rewrite an already-wired file")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != first.Updated {
		t.Fatal("the file on disk is not what the plan said it would write")
	}
}

// A file with no hooks: block is reported rather than guessed at.
func TestPlanHermesWithoutHooksBlockIsReported(t *testing.T) {
	path := writeFixture(t, "model:\n  provider: custom\n")
	plan, err := PlanHermes(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Complete() {
		t.Fatal("plan claims a config with no hooks block is complete")
	}
	joined := strings.Join(plan.Missing, "\n")
	if !strings.Contains(joined, "no hooks: block") {
		t.Fatalf("plan does not explain the missing hooks block: %v", plan.Missing)
	}
}

// The end-of-turn hook is Stop, not on_session_end. Wiring the natural name
// would make the session leave the room at the end of every turn.
func TestPlanHermesUsesStopForTheEndOfTurn(t *testing.T) {
	path := writeFixture(t, hermesFixture)
	plan, err := PlanHermes(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Updated, "--event=on_session_end") {
		t.Fatalf("the config wires on_session_end, which maps to leaving the room:\n%s", plan.Updated)
	}
	// It must still be attached to the on_session_end event key.
	if !strings.Contains(plan.Updated, "  on_session_end:") {
		t.Fatalf("on_session_end event key was not added:\n%s", plan.Updated)
	}
}

// Applying writes a backup, as every other config write in this tool does.
func TestApplyHermesPlanBacksUp(t *testing.T) {
	path := writeFixture(t, hermesFixture)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHermes(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyHermesPlan(plan, time.Now()); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(path + ".bak-agents-room-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	saved, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(before) {
		t.Fatal("the backup is not the original file")
	}
}

// Each hook lands under its own event, not all at the end of the block.
//
// The first version bounded an event's items with blockEnd, which stops only
// at a top-level key. Under hooks: every sibling event is indented, so the
// bound ran to the end of the whole block and every hook was attached to the
// last event -- visible only on a config that already had several events.
func TestPlanHermesAttachesEachHookToItsOwnEvent(t *testing.T) {
	fixture := `hooks:
  pre_tool_call:
    - command: /usr/local/bin/other-tool hook
      timeout: 20
  on_session_start:
    - command: /usr/local/bin/other-tool start
      timeout: 20
  on_session_finalize:
    - command: /usr/local/bin/other-tool end
      timeout: 20
`
	path := writeFixture(t, fixture)
	plan, err := PlanHermes(path)
	if err != nil {
		t.Fatal(err)
	}

	// Walk the result the way a reader would: an event's items are the lines
	// after its key and before the next key at the same indentation.
	lines := strings.Split(plan.Updated, "\n")
	eventOf := func(flag string) string {
		current := ""
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "-") {
				current = strings.TrimSuffix(trimmed, ":")
				continue
			}
			if strings.Contains(line, "--event="+flag) {
				return current
			}
		}
		return ""
	}
	for _, c := range []struct{ flag, event string }{
		{"pre_verify", "pre_verify"},
		{"pre_tool_call", "pre_tool_call"},
		{"on_session_start", "on_session_start"},
		{"pre_llm_call", "pre_llm_call"},
		{"Stop", "on_session_end"},
		{"on_session_finalize", "on_session_finalize"},
	} {
		if got := eventOf(c.flag); got != c.event {
			t.Errorf("hook --event=%s landed under %q, want %q\n---\n%s",
				c.flag, got, c.event, plan.Updated)
		}
	}
}
