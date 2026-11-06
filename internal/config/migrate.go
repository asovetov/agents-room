// Package config finds harness configuration files and migrates them when
// the command names change.
//
// The migration is deliberately conservative: it only rewrites the exact
// command names it knows, keeps a timestamped backup next to the original,
// and reports every file it touched. A file it cannot parse is reported and
// left alone.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Change is one rewrite that happened in one file.
type Change struct {
	File string `json:"file"`
	From string `json:"from"`
	To   string `json:"to"`
	Line int    `json:"line,omitempty"`
}

// Report is the outcome of a migration run.
type Report struct {
	Changes []Change `json:"changes"`
	Skipped []string `json:"skipped"`
}

// Changed reports whether anything was rewritten.
func (r Report) Changed() bool { return len(r.Changes) > 0 }

// replacements are the command renames, longest first so that a shorter name
// never eats part of a longer one.
var replacements = []struct{ from, to string }{
	{"agents-room-hook", "agents-room hook"},
	{"agents-room-mcp", "agents-room mcp"},
}

// Targets returns the configuration files worth looking at on this machine.
func Targets(home string) []string {
	candidates := []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.local.json"),
		filepath.Join(home, ".hermes", "config.yaml"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".codex", "hooks.json"),
	}
	out := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// Migrate rewrites old command names in the given files.
//
// A file is written only when something changed, and always after a backup
// copy named <file>.bak-agents-room-<timestamp>.
func Migrate(files []string, now time.Time) (Report, error) {
	var rep Report
	for _, path := range files {
		changes, err := migrateFile(path, now)
		if err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		rep.Changes = append(rep.Changes, changes...)
	}
	return rep, nil
}

func migrateFile(path string, now time.Time) ([]Change, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	original := string(data)

	// A JSON config is migrated through the parser so that formatting and
	// escaping stay valid. Everything else is text, because rewriting TOML
	// or YAML by hand risks losing comments.
	if strings.HasSuffix(path, ".json") {
		if strings.Contains(original, "agents-room-hook") || strings.Contains(original, "agents-room-mcp") {
			var v any
			if err := json.Unmarshal(data, &v); err != nil {
				return nil, fmt.Errorf("not valid JSON, left alone: %w", err)
			}
			rewritten, n := rewriteJSON(v)
			if n == 0 {
				return nil, nil
			}
			pretty, err := json.MarshalIndent(rewritten, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("encode JSON: %w", err)
			}
			pretty = append(pretty, '\n')
			if err := backupAndWrite(path, pretty, now); err != nil {
				return nil, err
			}
			return []Change{{File: path, From: "agents-room-hook/mcp", To: "agents-room hook/mcp"}}, nil
		}
		return nil, nil
	}

	changes := textChanges(original)
	if len(changes) == 0 {
		return nil, nil
	}
	updated := original
	for _, r := range replacements {
		updated = strings.ReplaceAll(updated, r.from, r.to)
	}
	if updated == original {
		return nil, nil
	}
	if err := backupAndWrite(path, []byte(updated), now); err != nil {
		return nil, err
	}
	for i := range changes {
		changes[i].File = path
	}
	return changes, nil
}

// rewriteJSON walks a decoded JSON value and rewrites command strings.
func rewriteJSON(v any) (any, int) {
	count := 0
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				updated := s
				for _, r := range replacements {
					updated = strings.ReplaceAll(updated, r.from, r.to)
				}
				if updated != s {
					t[k] = updated
					count++
				}
				continue
			}
			newVal, n := rewriteJSON(val)
			t[k] = newVal
			count += n
		}
		return t, count
	case []any:
		for i, val := range t {
			if s, ok := val.(string); ok {
				updated := s
				for _, r := range replacements {
					updated = strings.ReplaceAll(updated, r.from, r.to)
				}
				if updated != s {
					t[i] = updated
					count++
				}
				continue
			}
			newVal, n := rewriteJSON(val)
			t[i] = newVal
			count += n
		}
		return t, count
	default:
		return v, 0
	}
}

var commandLine = regexp.MustCompile(`(?m)^.*agents-room-(hook|mcp).*$`)

// textChanges lists the lines a text migration would rewrite.
func textChanges(s string) []Change {
	var out []Change
	for i, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "agents-room-hook") || strings.Contains(line, "agents-room-mcp") {
			out = append(out, Change{Line: i + 1, From: strings.TrimSpace(line)})
		}
	}
	return out
}

// backupAndWrite copies the original aside and writes the new content.
func backupAndWrite(path string, content []byte, now time.Time) error {
	backup := fmt.Sprintf("%s.bak-agents-room-%s", path, now.Format("20060102-150405"))
	if err := os.WriteFile(backup, mustRead(path), 0o600); err != nil {
		return fmt.Errorf("back up %s: %w", path, err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func mustRead(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// StaleCommandPattern is what a doctor check looks for.
func StaleCommandPattern() *regexp.Regexp { return commandLine }
