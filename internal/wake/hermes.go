package wake

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// runWake builds the command that resumes a session. A variable so a test can
// see what would be spawned without starting an agent.
var runWake = func(bin, key, line string) *exec.Cmd {
	return exec.Command(bin, "--resume", key, "-z", line)
}

// HermesCLI resumes a Hermes session with a one-shot prompt, which starts a
// turn in a chat that is otherwise idle.
//
// This is the only lever Hermes has: it sets no inbox socket, so the socket
// backend cannot reach it, and without this its messages wait for the next
// time the agent happens to run.
//
// It is also not a signal. `hermes -z` sets HERMES_YOLO_MODE=1, so the turn
// it starts runs commands without asking. That is why nothing here runs unless
// the operator turned it on; see Enabled.
func HermesCLI(harnessKey, line string) error {
	if harnessKey == "" {
		return fmt.Errorf("hermes wake: no harness key")
	}
	bin := hermesBinary()
	if bin == "" {
		return fmt.Errorf("hermes wake: the hermes binary was not found (set HERMES_BIN or put hermes on PATH)")
	}

	cmd := runWake(bin, harnessKey, line)
	// The woken turn must be the same session, or it reads an empty inbox.
	//
	// Hermes exports HERMES_SESSION_ID to a turn's children but the process we
	// start has none: its session-start hook then registers a new, nameless
	// session, while the agent's own msg_recv runs under the resumed id. The
	// message stays `accepted` for ever and the reply comes from the other
	// row -- seen live: the woken turn reported "0 unread" while the message
	// sat on the session it was meant for. Setting the variable here makes the
	// hook rejoin the session that was woken, so the read lands on it.
	cmd.Env = append(os.Environ(),
		"HERMES_SESSION_ID="+harnessKey,
		// The hook must know this turn is a wake, not the end of the session:
		// `-z` fires on_session_end when its one turn finishes, and treating
		// that as "the chat is over" deletes the session that was woken.
		"AGENTS_ROOM_WAKE=1",
	)
	// The turn is not this process's to wait for: the hook that emits it has a
	// time budget of seconds and the turn can run for minutes. Its output goes
	// to a log rather than the harness's stdout, which is a channel the
	// harness parses.
	if logPath := hermesWakeLog(harnessKey); logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
		}
	}
	cmd.Stdin = nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("hermes wake: start: %w", err)
	}
	// Reap it in the background; a zombie per wake would accumulate for the
	// life of the MCP server.
	go func() { _ = cmd.Wait() }()
	return nil
}

// hermesBinary returns the Hermes executable to run.
func hermesBinary() string {
	if p := strings.TrimSpace(os.Getenv("HERMES_BIN")); p != "" {
		return p
	}
	p, err := exec.LookPath("hermes")
	if err != nil {
		return ""
	}
	return p
}

// enabledPath is where the operator's decision lives.
//
// A file rather than a store row or a new command: it is one line the
// operator writes and deletes, it needs no schema change, and it is visible
// in the same directory as the database. A missing file means off.
func enabledPath() string {
	if p := strings.TrimSpace(os.Getenv("AGENTS_ROOM_WAKE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents-room", "wake")
}

// Enabled reports whether the operator allowed Hermes sessions to be woken.
//
// Accepted spellings, one per line, "#" comments allowed: "hermes",
// "hermes=cli", "cli". Anything else leaves it off.
func Enabled() bool {
	path := enabledPath()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "hermes", "cli", "hermes=cli", "hermes-cli":
			return true
		}
	}
	return false
}

// singleFlight keeps a burst of messages from starting a turn per message.
//
// A wake is a whole agent turn, and several messages sent within a minute are
// normally one request. The window is per session, so one busy recipient does
// not silence another.
var singleFlight = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

// singleFlightWindow is how long a session is left alone after a wake.
//
// It is deliberately longer than a hook's retry window: the point is to let
// the turn it started finish before another one is stacked on top.
var singleFlightWindow = 60 * time.Second

// AllowWake reports whether a wake may be emitted for this session now, and
// records it if so.
func AllowWake(sessionID string) bool {
	now := time.Now()
	singleFlight.mu.Lock()
	defer singleFlight.mu.Unlock()
	if last, ok := singleFlight.last[sessionID]; ok && now.Sub(last) < singleFlightWindow {
		return false
	}
	singleFlight.last[sessionID] = now
	return true
}

// resetSingleFlight clears the window; tests use it.
func resetSingleFlight() {
	singleFlight.mu.Lock()
	defer singleFlight.mu.Unlock()
	singleFlight.last = map[string]time.Time{}
}

// hermesWakeLog is where a woken turn's output goes, so a failure is
// diagnosable without the harness's stdout.
func hermesWakeLog(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents-room", "wake-"+sanitize(sessionID)+".log")
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
