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
	fix := fs.Bool("fix", false, "apply the repairs doctor can make on its own")
	dryRun := fs.Bool("dry-run", false, "with --fix, show what would change and stop")
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
		dups := h.DuplicatePIDs(ctx)
		for _, dup := range dups {
			line(false, "%s %s has %d sessions (%s); they are one chat split into several, so room_who overcounts. "+
				"Update, rejoin with `--key`, then leave the extras",
				dup.Kind, dup.Identity, len(dup.IDs), strings.Join(dup.IDs, ", "))
		}
		if len(dups) > 0 {
			runDuplicateFix(ctx, h, stdout, stderr, *fix, *dryRun, &problems, line)
		}
		// Read after the duplicate fix: it can collapse two rows into one, and
		// reporting the rows it removed as "gone sessions to prune" would send
		// the operator after sessions that no longer exist.
		reportGoneSessions(ctx, h, stdout, line)
		reportUnhookedSession(ctx, h, sessions, stdout, line)
		reportQuietAutonomy(sessions, line)
		// A lease that lapses under a session that is still working is the
		// case the TTL is meant to prevent, and nothing else can see it
		// coming. The hook renews on every tool event, so a warning here
		// usually means the harness is not calling that hook.
		for _, s := range sessions {
			expiring, err := h.ExpiringLeases(ctx, s.ID, 0.25)
			if err != nil {
				continue
			}
			now := time.Now().Unix()
			for _, l := range expiring {
				line(false, "%s holds %s but its lease ends in %s; the hook renews leases on every tool event, "+
					"so this session is probably not running it",
					sessionName(s), l.Path, humanUntil(l.ExpiresAt, now))
			}
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

// runDuplicateFix reports, and optionally applies, the removal of sessions
// that registered one chat more than once.
//
// This is the one repair doctor makes that deletes rows, so it is opt-in
// twice: --fix is needed to do anything, and --dry-run shows the plan without
// touching the store. What it removes is decided by PlanDuplicateFix, not
// here, so the reasoning lives with the data.
func runDuplicateFix(ctx context.Context, h *hub.Hub, stdout, stderr io.Writer,
	fix, dryRun bool, problems *int, line func(bool, string, ...any)) {

	plan, err := h.PlanDuplicateFix(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: plan duplicate fix: %v\n", err)
		return
	}
	for _, f := range plan {
		// What ties the rows together is sometimes a key and sometimes a pid,
		// so the plan says which, rather than always claiming a pid.
		fmt.Fprintf(stdout, "     %s %s: keep %s (%s), remove %s",
			f.Kind, f.Identity, f.Keep, f.Because, strings.Join(f.Remove, ", "))
		if f.Moved > 0 {
			// The count matters before the fix runs: it is the number of
			// messages that would be lost if the rows were simply deleted.
			fmt.Fprintf(stdout, "; %d unread delivery(ies) move to the kept session", f.Moved)
		}
		fmt.Fprintln(stdout)
	}
	if !fix {
		fmt.Fprintln(stdout, "     rerun with --fix to apply, or --fix --dry-run to see it without changing anything")
		return
	}
	if dryRun {
		fmt.Fprintln(stdout, "     --dry-run: nothing changed")
		return
	}

	n, err := h.ApplyDuplicateFix(ctx, plan)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return
	}
	// The problem is gone, so it must not still be counted as one.
	if *problems > 0 {
		*problems--
	}
	fmt.Fprintf(stdout, "     removed %d duplicate session(s)\n", n)
}

// reportGoneSessions names rows whose chat is no longer running.
//
// It is the one thing doctor could not see before: every other check reads
// the store, and the store cannot tell a thinking agent from a dead one. The
// row is not a problem with the installation, so it does not count toward the
// exit status; it is housekeeping the operator can choose to do, and the
// command that does it is named.
func reportGoneSessions(ctx context.Context, h *hub.Hub, stdout io.Writer, line func(bool, string, ...any)) {
	gone := 0
	names := []string{}
	for _, room := range h.RoomNames(ctx) {
		sessions, err := h.Who(ctx, room)
		if err != nil {
			continue
		}
		for _, s := range sessions {
			// Rooms overlap, so one session is met in each of its rooms.
			if s.Liveness != hub.LivenessGone {
				continue
			}
			name := sessionName(s)
			already := false
			for _, seen := range names {
				if seen == name {
					already = true
					break
				}
			}
			if !already {
				names = append(names, name)
				gone++
			}
		}
	}
	if gone == 0 {
		return
	}
	fmt.Fprintf(stdout, "     %d session(s) whose chat is no longer running: %s\n",
		gone, strings.Join(names, ", "))
	fmt.Fprintln(stdout, "     `agents-room prune --older-than 1` removes them once they have been quiet that long")
}

// reportUnhookedSession names a live session that has stopped reporting tool
// events.
//
// A session calls Touch on every tool call, so last_seen moves while the agent
// works. A session whose chat is alive but whose last_seen has not moved for a
// long time is running without the hook: leases lapse under it, wake signals
// reach nobody, and every symptom looks like a room problem rather than a
// configuration one. This is the most common broken install and nothing else
// in doctor can see it.
func reportUnhookedSession(ctx context.Context, h *hub.Hub, sessions []hub.Session, stdout io.Writer, line func(bool, string, ...any)) {
	const quiet = 2 * time.Hour
	now := time.Now().Unix()
	for _, s := range sessions {
		if s.Liveness != hub.LivenessAlive {
			continue
		}
		idle := now - s.LastSeen
		if idle < int64(quiet.Seconds()) {
			continue
		}
		line(false, "%s is running but has reported no tool call for %s; the hook is "+
			"probably not configured for this harness, so its leases lapse and wake "+
			"signals reach nobody. See docs/harness-setup.md",
			sessionName(s), time.Duration(idle)*time.Second)
	}
}

// reportQuietAutonomy names a live session that cannot be woken while idle.
//
// Below wake the signal rides the current turn: it arrives only when the agent
// is already doing something. An idle chat at notify or hold is therefore
// unreachable, which looks exactly like a broken hook and is invisible
// everywhere else -- the room shows the session as alive, the wake signal is
// reported as available, and the message simply waits.
func reportQuietAutonomy(sessions []hub.Session, line func(bool, string, ...any)) {
	for _, s := range sessions {
		if s.Liveness != hub.LivenessAlive || s.Autonomy >= hub.AutonomyWake {
			continue
		}
		line(false, "%s is alive but its autonomy is %d, so it will not be woken "+
			"while idle: a signal only rides a turn it is already taking. A rejoin "+
			"can raise it to 2; it can never lower it",
			sessionName(s), s.Autonomy)
	}
}

// sessionName is what a human calls a session: its label when it has one,
// its id otherwise.
func sessionName(s hub.Session) string {
	if s.Label != "" {
		return s.Label
	}
	return s.ID
}

// humanUntil renders the time left before a unix timestamp, rounded to the
// minute, for a report a person reads.
func humanUntil(expiresAt, now int64) string {
	left := time.Duration(expiresAt-now) * time.Second
	if left < time.Minute {
		return "less than a minute"
	}
	return left.Round(time.Minute).String()
}

// splitSharedRoom is one unprefixed room that duplicates a shared one.
type splitSharedRoom struct {
	Bare     string
	Shared   string
	Stranded int
}

// splitSharedRooms finds rooms that exist both as "name" and "shared:name".
//
// Before join normalised the prefix, `--also storefront` created a second room
// beside `shared:storefront`. Nothing merges them: a session in the wrong one
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
		// Only a session in the bare room and nowhere else is stranded. One
		// that also joined the shared room is reachable there, and counting
		// it would send the operator after a session that needs nothing.
		stranded := 0
		for _, s := range sessions {
			if !contains(s.Rooms, shared) {
				stranded++
			}
		}
		if stranded == 0 {
			continue
		}
		out = append(out, splitSharedRoom{Bare: name, Shared: shared, Stranded: stranded})
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
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
