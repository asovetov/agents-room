package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

var when = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func TestMigrateJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	write(t, path, `{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "agents-room-hook --event=SessionStart"}]}
    ]
  },
  "mcpServers": {
    "agents-room": {"command": "agents-room-mcp", "args": []}
  },
  "theme": "dark"
}`)

	rep, err := Migrate([]string{path}, when)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !rep.Changed() {
		t.Fatal("nothing was rewritten")
	}

	var got map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	cmd := got["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
	if cmd != "agents-room hook --event=SessionStart" {
		t.Fatalf("hook command = %q", cmd)
	}

	mcp := got["mcpServers"].(map[string]any)["agents-room"].(map[string]any)["command"].(string)
	if mcp != "agents-room mcp" {
		t.Fatalf("mcp command = %q", mcp)
	}

	// Unrelated settings survive.
	if got["theme"] != "dark" {
		t.Fatalf("unrelated setting lost: %v", got["theme"])
	}

	// A backup exists.
	matches, _ := filepath.Glob(path + ".bak-agents-room-*")
	if len(matches) != 1 {
		t.Fatalf("got %d backups, want 1", len(matches))
	}
}

func TestMigrateYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, `hooks:
  pre_llm_call:
    - command: agents-room-hook --harness=hermes --event=pre_llm_call
      timeout: 10
other:
  keep: me
`)

	if _, err := Migrate([]string{path}, when); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if strings.Contains(text, "agents-room-hook") {
		t.Fatalf("old name survived:\n%s", text)
	}
	if !strings.Contains(text, "agents-room hook --harness=hermes") {
		t.Fatalf("new name missing:\n%s", text)
	}
	// Comments and unrelated keys are untouched because the file is edited
	// as text.
	if !strings.Contains(text, "keep: me") || !strings.Contains(text, "timeout: 10") {
		t.Fatalf("unrelated content lost:\n%s", text)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, "command: agents-room-hook --event=Stop\n")

	if _, err := Migrate([]string{path}, when); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)

	rep, err := Migrate([]string{path}, when.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed() {
		t.Fatal("a second run rewrote an already migrated file")
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("file changed on a second run")
	}
}

func TestMigrateLeavesInvalidJSONAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	broken := `{"hooks": {"SessionStart": [ agents-room-hook `
	write(t, path, broken)

	rep, err := Migrate([]string{path}, when)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rep.Changed() {
		t.Fatal("an unparseable file was rewritten")
	}
	if len(rep.Skipped) != 1 {
		t.Fatalf("skipped = %v, want one entry", rep.Skipped)
	}
	data, _ := os.ReadFile(path)
	if string(data) != broken {
		t.Fatal("an unparseable file was modified")
	}
}

func TestMigrateIgnoresCleanFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	write(t, path, `{"hooks": {}, "command": "agents-room hook --event=Stop"}`)

	rep, err := Migrate([]string{path}, when)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed() {
		t.Fatal("a clean file was rewritten")
	}
	matches, _ := filepath.Glob(path + ".bak-agents-room-*")
	if len(matches) != 0 {
		t.Fatalf("a backup was made for an unchanged file: %v", matches)
	}
}

func TestTargetsOnlyReturnsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".claude", "settings.json"), "{}")

	got := Targets(dir)
	if len(got) != 1 {
		t.Fatalf("targets = %v, want only the file that exists", got)
	}
}
