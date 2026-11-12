package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HermesHooks are the events a Hermes install needs agents-room wired into.
//
// on_session_end carries --event=Stop, not on_session_end: the hook maps that
// event to "leave the room", so wiring the natural name would make the
// session drop out of the room at the end of every turn.
var HermesHooks = []struct{ Event, Flag string }{
	{"pre_verify", "pre_verify"},
	{"pre_tool_call", "pre_tool_call"},
	{"on_session_start", "on_session_start"},
	{"pre_llm_call", "pre_llm_call"},
	{"on_session_end", "Stop"},
	{"on_session_finalize", "on_session_finalize"},
}

// HermesPlan describes what a Hermes config is missing and what would be
// written.
type HermesPlan struct {
	Path string
	// Missing lists the pieces that are absent, in the order they are
	// reported. Empty means the file is already wired up.
	Missing []string
	// Updated is the file with the missing pieces spliced in.
	Updated string
}

// Complete reports whether the config needs nothing.
func (p HermesPlan) Complete() bool { return len(p.Missing) == 0 }

// PlanHermes inspects ~/.hermes/config.yaml and returns the splice that
// would wire agents-room in.
//
// The file is user-owned and full of comments, so it is edited as text: the
// missing lines are inserted next to the block they belong to and nothing
// else is reformatted. Re-running on an already-wired file is a no-op, which
// is what makes `doctor --fix` safe to run repeatedly.
func PlanHermes(path string) (HermesPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HermesPlan{}, err
	}
	text := string(data)
	plan := HermesPlan{Path: path}
	updated := text

	command := hermesHookCommand()
	for _, h := range HermesHooks {
		want := fmt.Sprintf("--harness=hermes --event=%s", h.Flag)
		if strings.Contains(updated, want) {
			continue
		}
		next, ok := spliceHook(updated, h.Event, fmt.Sprintf("%s hook %s --autonomy=2", command, want))
		if !ok {
			plan.Missing = append(plan.Missing, fmt.Sprintf("hook %s (no hooks: block to extend)", h.Event))
			continue
		}
		updated = next
		plan.Missing = append(plan.Missing, "hook "+h.Event)
	}

	if !hooksAutoAccept(updated) {
		next, ok := setHooksAutoAccept(updated)
		if ok {
			updated = next
			plan.Missing = append(plan.Missing, "hooks_auto_accept: true")
		}
	}

	if !strings.Contains(updated, "agents-room:") {
		next, ok := spliceMCPServer(updated)
		if ok {
			updated = next
			plan.Missing = append(plan.Missing, "mcp_servers.agents-room")
		}
	}

	plan.Updated = updated
	return plan, nil
}

// hermesHookCommand is the command written into the config.
//
// An absolute path is used when this binary can name itself, because the
// environment a Hermes hook runs in does not guarantee the operator's PATH.
func hermesHookCommand() string {
	if exe, err := os.Executable(); err == nil {
		if base := filepath.Base(exe); strings.HasPrefix(base, "agents-room") {
			return exe
		}
	}
	return "agents-room"
}

// spliceHook adds one command under an event in the hooks: block, creating
// the event key when it is absent. It reports false when there is no hooks:
// block to extend, which is the one case a text splice cannot handle without
// inventing structure.
func spliceHook(text, event, command string) (string, bool) {
	lines := strings.Split(text, "\n")
	hooksAt := indexTopLevel(lines, "hooks:")
	if hooksAt < 0 {
		return text, false
	}
	hooksEnd := blockEnd(lines, hooksAt)

	item := []string{
		"    - command: " + command,
		"      timeout: 10",
	}

	// The event key already exists: append at the end of its items, which is
	// the last indented line before the next event key -- not blockEnd, which
	// would run past the following sibling keys and attach this hook to the
	// wrong event.
	if evAt := indexChild(lines, hooksAt, hooksEnd, "  "+event+":"); evAt >= 0 {
		evEnd := eventBlockEnd(lines, evAt, hooksEnd)
		out := append([]string{}, lines[:evEnd]...)
		out = append(out, item...)
		out = append(out, lines[evEnd:]...)
		return strings.Join(out, "\n"), true
	}

	// Otherwise add a new event block at the end of the hooks block.
	block := append([]string{"  " + event + ":"}, item...)
	out := append([]string{}, lines[:hooksEnd]...)
	out = append(out, block...)
	out = append(out, lines[hooksEnd:]...)
	return strings.Join(out, "\n"), true
}

// hooksAutoAccept reports whether the config already accepts hooks without a
// prompt. Hermes will not run them headless otherwise.
func hooksAutoAccept(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "hooks_auto_accept:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "hooks_auto_accept:")) == "true"
		}
	}
	return false
}

// setHooksAutoAccept turns the flag on, adding it at the top level when it is
// absent.
func setHooksAutoAccept(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "hooks_auto_accept:") && !strings.HasPrefix(line, " ") {
			lines[i] = "hooks_auto_accept: true"
			return strings.Join(lines, "\n"), true
		}
	}
	// Place it right after the hooks block, where a reader expects it.
	if hooksAt := indexTopLevel(lines, "hooks:"); hooksAt >= 0 {
		at := blockEnd(lines, hooksAt)
		out := append([]string{}, lines[:at]...)
		out = append(out, "hooks_auto_accept: true")
		out = append(out, lines[at:]...)
		return strings.Join(out, "\n"), true
	}
	return text, false
}

// spliceMCPServer adds agents-room to mcp_servers.
func spliceMCPServer(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	at := indexTopLevel(lines, "mcp_servers:")
	if at < 0 {
		// No MCP section at all: append one at the end, which is where a
		// hand-edited config usually keeps it too.
		out := append([]string{}, lines...)
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out,
			"mcp_servers:",
			"  agents-room:",
			"    command: agents-room",
			"    args: [mcp]",
		)
		return strings.Join(out, "\n"), true
	}
	end := blockEnd(lines, at)
	block := []string{
		"  agents-room:",
		"    command: agents-room",
		"    args: [mcp]",
	}
	out := append([]string{}, lines[:end]...)
	out = append(out, block...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), true
}

// indexTopLevel finds a line that starts a top-level key.
func indexTopLevel(lines []string, key string) int {
	for i, line := range lines {
		if strings.HasPrefix(line, key) {
			return i
		}
	}
	return -1
}

// indexChild finds a key indented exactly as given, inside [from, to).
func indexChild(lines []string, from, to int, key string) int {
	for i := from + 1; i < to; i++ {
		if strings.HasPrefix(lines[i], key) {
			return i
		}
	}
	return -1
}

// blockEnd returns the first index after start whose line is not blank and
// not indented, which is where a top-level block stops.
func blockEnd(lines []string, start int) int {
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			return i
		}
	}
	return len(lines)
}

// eventBlockEnd returns the first index after start that is not part of the
// event's own items: the next sibling key at the same indentation, a
// shallower line, or the end of the enclosing block.
//
// blockEnd is the wrong bound here. Under `hooks:` every event is an indented
// key, so a sibling event is still "indented" and blockEnd would run to the
// end of the whole hooks block, appending a hook to the last event instead of
// the one asked for.
func eventBlockEnd(lines []string, start, limit int) int {
	indent := len(lines[start]) - len(strings.TrimLeft(lines[start], " "))
	for i := start + 1; i < limit; i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		lead := len(line) - len(strings.TrimLeft(line, " "))
		if lead <= indent {
			return i
		}
	}
	return limit
}

// ApplyHermesPlan writes the plan, backing the file up first.
//
// The backup name is the one the rest of the tool uses, so an operator who
// already knows `migrate-config` recognises it.
func ApplyHermesPlan(p HermesPlan, now time.Time) error {
	if p.Complete() {
		return nil
	}
	return backupAndWrite(p.Path, []byte(p.Updated), now)
}
