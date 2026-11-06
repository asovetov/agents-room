package main

import (
	"encoding/json"
	"testing"

	"github.com/asovetov/agents-room/internal/hub"
)

func TestNormalizeEvent(t *testing.T) {
	cases := []struct {
		harness, event, want string
	}{
		{"claude-code", "SessionStart", eventJoin},
		{"claude-code", "SessionEnd", eventLeave},
		{"claude-code", "Stop", eventNudge},
		{"claude-code", "PostToolBatch", eventNudge},
		{"claude-code", "Notification", ""},
		{"hermes", "on_session_start", eventJoin},
		{"hermes", "on_session_finalize", eventLeave},
		{"hermes", "pre_llm_call", eventNudge},
		{"hermes", "pre_verify", eventNudge},
		{"hermes", "post_tool_call", ""},
		{"codex", "SessionStart", eventJoin},
		{"codex", "Stop", eventNudge},
	}
	for _, c := range cases {
		if got := normalizeEvent(c.harness, c.event); got != c.want {
			t.Errorf("normalizeEvent(%q, %q) = %q, want %q", c.harness, c.event, got, c.want)
		}
	}
}

func TestClaudeCodeStopBlocksOnlyAtHoldOrAbove(t *testing.T) {
	line := "[agents-room] 1 unread; call msg_recv"

	notify := outputFor("claude-code", "Stop", hub.AutonomyNotify, line)
	if notify != "" {
		t.Fatalf("notify level produced output %q, want silence", notify)
	}

	held := outputFor("claude-code", "Stop", hub.AutonomyHold, line)
	var out map[string]any
	if err := json.Unmarshal([]byte(held), &out); err != nil {
		t.Fatalf("stop output is not JSON: %v", err)
	}
	if out["decision"] != "block" {
		t.Fatalf("decision = %v, want block", out["decision"])
	}
	if out["reason"] != line {
		t.Fatalf("reason = %v, want the nudge line", out["reason"])
	}
}

func TestClaudeCodeUserPromptSubmitNestsContext(t *testing.T) {
	line := "[agents-room] 2 unread; call msg_recv"
	raw := outputFor("claude-code", "UserPromptSubmit", hub.AutonomyNotify, line)

	var out struct {
		HookSpecific struct {
			EventName string `json:"hookEventName"`
			Context   string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	// A top-level additionalContext is silently ignored by Claude Code, so
	// the nesting is the contract, not a style choice.
	if out.HookSpecific.EventName != "UserPromptSubmit" {
		t.Fatalf("hookEventName = %q", out.HookSpecific.EventName)
	}
	if out.HookSpecific.Context != line {
		t.Fatalf("additionalContext = %q, want the nudge line", out.HookSpecific.Context)
	}
}

func TestHermesOutputs(t *testing.T) {
	line := "[agents-room] 1 unread; call msg_recv"

	var ctxOut map[string]any
	if err := json.Unmarshal([]byte(outputFor("hermes", "pre_llm_call", hub.AutonomyNotify, line)), &ctxOut); err != nil {
		t.Fatalf("hermes pre_llm_call output: %v", err)
	}
	if ctxOut["context"] != line {
		t.Fatalf("context = %v, want the nudge line", ctxOut["context"])
	}

	// Hermes pre_verify holds only when autonomy allows it.
	if got := outputFor("hermes", "pre_verify", hub.AutonomyNotify, line); got != "" {
		t.Fatalf("notify produced %q on pre_verify, want silence", got)
	}
	var verify map[string]any
	if err := json.Unmarshal([]byte(outputFor("hermes", "pre_verify", hub.AutonomyHold, line)), &verify); err != nil {
		t.Fatalf("hermes pre_verify output: %v", err)
	}
	if verify["action"] != "continue" || verify["message"] != line {
		t.Fatalf("pre_verify = %v, want action continue with the line", verify)
	}
}

func TestCodexOutputIsPlainText(t *testing.T) {
	line := "[agents-room] 1 unread; call msg_recv"
	if got := outputFor("codex", "Stop", hub.AutonomyNotify, line); got != line {
		t.Fatalf("codex output = %q, want the bare line", got)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("  0123456789abcdef  "); got != "01234567" {
		t.Fatalf("shortID = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Fatalf("shortID = %q", got)
	}
}
