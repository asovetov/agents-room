package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asovetov/agents-room/internal/config"
	"github.com/asovetov/agents-room/internal/hub"
	"github.com/asovetov/agents-room/internal/update"
)

// runUpdate checks GitHub for a newer release and installs it on request.
//
// Nothing is replaced without confirmation: --check prints the result and
// stops, and an interactive run asks before touching the binary.
func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkOnly := fs.Bool("check", false, "only report whether a newer release exists")
	yes := fs.Bool("yes", false, "install without asking")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client := update.NewClient()
	rel, err := client.Latest(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	if !update.IsNewer(Version, rel.Tag) {
		fmt.Fprintf(stdout, "up to date: %s (latest release is %s)\n", Version, rel.Tag)
		return 0
	}

	fmt.Fprintf(stdout, "new release: %s (you have %s)\n\n", rel.Tag, Version)
	if rel.Body != "" {
		fmt.Fprintln(stdout, strings.TrimSpace(firstParagraph(rel.Body)))
		fmt.Fprintln(stdout)
	}
	if *checkOnly {
		fmt.Fprintln(stdout, "run `agents-room update` to install it")
		return 0
	}

	goos, goarch := update.Platform()
	assetName := update.AssetFor(goos, goarch)
	asset, ok := rel.FindAsset(assetName)
	if !ok {
		return fail(stderr, fmt.Errorf("release %s has no asset for %s/%s", rel.Tag, goos, goarch))
	}
	sums, okSums := rel.FindAsset("SHA256SUMS")
	sig, okSig := rel.FindAsset("SHA256SUMS.asc")
	if !okSums || !okSig {
		return fail(stderr, fmt.Errorf("release %s is not signed: SHA256SUMS or SHA256SUMS.asc is missing", rel.Tag))
	}

	target, err := os.Executable()
	if err != nil {
		return fail(stderr, fmt.Errorf("find this binary: %w", err))
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return fail(stderr, fmt.Errorf("resolve this binary: %w", err))
	}

	if !*yes {
		fmt.Fprintf(stdout, "Replace %s with %s? [y/N] ", target, rel.Tag)
		if !confirm(os.Stdin) {
			fmt.Fprintln(stdout, "not changed")
			return 0
		}
	}

	dir, err := os.MkdirTemp("", "agents-room-update-")
	if err != nil {
		return fail(stderr, err)
	}
	defer os.RemoveAll(dir)

	binPath, err := client.Download(ctx, asset, dir)
	if err != nil {
		return fail(stderr, err)
	}
	sumsPath, err := client.Download(ctx, sums, dir)
	if err != nil {
		return fail(stderr, err)
	}
	sigPath, err := client.Download(ctx, sig, dir)
	if err != nil {
		return fail(stderr, err)
	}

	// Signature first: a checksum from a tampered file proves nothing.
	if err := update.VerifySignature(sumsPath, sigPath); err != nil {
		return fail(stderr, fmt.Errorf("refusing to install: %w", err))
	}
	fmt.Fprintln(stdout, "signature ok")
	if err := update.VerifyChecksum(binPath, sumsPath); err != nil {
		return fail(stderr, fmt.Errorf("refusing to install: %w", err))
	}
	fmt.Fprintln(stdout, "checksum ok")

	if err := update.Install(binPath, target); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "installed %s\n", rel.Tag)

	// Harness configuration is migrated as part of the update, because the
	// command names changed and an old name would leave hooks silently
	// failing.
	home, err := os.UserHomeDir()
	if err == nil {
		rep, err := config.Migrate(config.Targets(home), time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "config migration: %v\n", err)
		}
		reportConfig(stdout, stderr, rep)
	}
	return 0
}

// runMigrateConfig rewrites old command names without updating the binary.
func runMigrateConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "show what would change without writing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fail(stderr, err)
	}
	targets := config.Targets(home)
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "no harness configuration found")
		return 0
	}

	if *dryRun {
		for _, path := range targets {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if strings.Contains(string(data), "agents-room-hook") || strings.Contains(string(data), "agents-room-mcp") {
				fmt.Fprintf(stdout, "would rewrite %s\n", path)
			}
		}
		return 0
	}

	rep, err := config.Migrate(targets, time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	reportConfig(stdout, stderr, rep)
	return 0
}

func reportConfig(stdout, stderr io.Writer, rep config.Report) {
	for _, c := range rep.Changes {
		fmt.Fprintf(stdout, "updated %s\n", c.File)
	}
	for _, s := range rep.Skipped {
		fmt.Fprintf(stderr, "skipped %s\n", s)
	}
	if !rep.Changed() && len(rep.Skipped) == 0 {
		fmt.Fprintln(stdout, "harness configuration is already current")
	}
}

// runDoctor reports what is installed and wired up.
func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	problems := 0
	line := func(ok bool, format string, a ...any) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			problems++
		}
		fmt.Fprintf(stdout, "%s %s\n", mark, fmt.Sprintf(format, a...))
	}

	goos, goarch := update.Platform()
	fmt.Fprintf(stdout, "agents-room %s (%s/%s)\n\n", Version, goos, goarch)
	fmt.Fprintln(stdout, "Store")

	h, err := hub.Open()
	if err != nil {
		line(false, "store opens: %v", err)
	} else {
		defer h.Close()
		line(true, "store at %s", h.Path())
		if v, err := storeVersion(h); err == nil {
			line(true, "schema version %d", v)
		}
		room := hub.RepoRoom("")
		sessions, err := h.Who(ctx, room)
		if err != nil {
			line(false, "read room %s: %v", room, err)
		} else {
			line(true, "room %s has %d session(s)", room, len(sessions))
		}
		for _, split := range splitSharedRooms(ctx, h) {
			line(false, "%s and %s are two rooms; %d session(s) sit in the unprefixed one and are unreachable from the other. "+
				"Each of them rejoins once, then runs `agents-room leave --session <id> --room %s`",
				split.Bare, split.Shared, split.Stranded, split.Bare)
		}
	}

	fmt.Fprintln(stdout, "\nHarness configuration")
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(stderr, err)
	}
	targets := config.Targets(home)
	if len(targets) == 0 {
		line(false, "no harness configuration found")
	}
	stale := config.StaleCommandPattern()
	for _, path := range targets {
		data, err := os.ReadFile(path)
		if err != nil {
			line(false, "read %s: %v", path, err)
			continue
		}
		text := string(data)
		hasOurs := strings.Contains(text, "agents-room")
		hasStale := stale.MatchString(text)
		switch {
		case hasStale:
			line(false, "%s uses an old command name; run `agents-room migrate-config`", path)
		case hasOurs:
			line(true, "%s is wired up", path)
		default:
			line(true, "%s has no agents-room entry", path)
		}
	}

	fmt.Fprintln(stdout, "\nWake signal")
	if p := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"); p != "" {
		line(true, "Claude Code inbox socket available")
	} else {
		line(true, "no Claude Code inbox socket in this shell; hooks still run, idle wake is unavailable")
	}

	fmt.Fprintln(stdout)
	if problems == 0 {
		fmt.Fprintln(stdout, "no problems found")
		return 0
	}
	fmt.Fprintf(stdout, "%d problem(s) found\n", problems)
	return 1
}

// splitSharedRoom is one unprefixed room that duplicates a shared one.
type splitSharedRoom struct {
	Bare     string
	Shared   string
	Stranded int
}

// splitSharedRooms finds rooms that exist both as "name" and "shared:name".
//
// Before join normalised the prefix, `--also aerobasis` created a second room
// beside `shared:aerobasis`. Nothing merges them: a session in the wrong one
// is invisible in the right one, so the operator has to be told which name is
// the orphan and how many sessions sit in it.
func splitSharedRooms(ctx context.Context, h *hub.Hub) []splitSharedRoom {
	var out []splitSharedRoom
	for _, name := range h.RoomNames(ctx) {
		if strings.Contains(name, ":") {
			continue
		}
		shared := "shared:" + name
		if !h.HasRoom(ctx, shared) {
			continue
		}
		sessions, err := h.Who(ctx, name)
		if err != nil || len(sessions) == 0 {
			continue
		}
		out = append(out, splitSharedRoom{Bare: name, Shared: shared, Stranded: len(sessions)})
	}
	return out
}

func storeVersion(h *hub.Hub) (int, error) {
	return h.SchemaVersion(context.Background())
}

// firstParagraph returns the first paragraph of a release body.
func firstParagraph(body string) string {
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block != "" && !strings.HasPrefix(block, "#") {
			return block
		}
	}
	return ""
}

// confirm reads a yes answer from r. Anything but y or yes is a no.
func confirm(r io.Reader) bool {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
	case "y", "yes", "да", "д":
		return true
	}
	return false
}
