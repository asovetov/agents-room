// Package cli implements the agents-room command line.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/asovetov/agents-room/internal/hook"
	"github.com/asovetov/agents-room/internal/hub"
	"github.com/asovetov/agents-room/internal/mcpserver"
)

// Run dispatches a subcommand and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	ctx := context.Background()
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "join":
		return runJoin(ctx, rest, stdout, stderr)
	case "leave":
		return runLeave(ctx, rest, stdout, stderr)
	case "who":
		return runWho(ctx, rest, stdout, stderr)
	case "cap":
		return runCap(ctx, rest, stdout, stderr)
	case "send":
		return runSend(ctx, rest, stdout, stderr)
	case "recv":
		return runRecv(ctx, rest, stdout, stderr)
	case "ack":
		return runAck(ctx, rest, stdout, stderr)
	case "lease":
		return runLease(ctx, rest, stdout, stderr)
	case "drift":
		return runDrift(ctx, rest, stdout, stderr)
	case "watch":
		return runWatch(ctx, rest, stdout, stderr)
	case "prune":
		return runPrune(ctx, rest, stdout, stderr)
	case "mcp":
		return runMCP(stdout, stderr)
	case "hook":
		return hook.Run(rest)
	case "update":
		return runUpdate(ctx, rest, stdout, stderr)
	case "migrate-config":
		return runMigrateConfig(rest, stdout, stderr)
	case "doctor":
		return runDoctor(ctx, rest, stdout, stderr)
	case "path":
		return runPath(stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, Version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

// Version is set at build time with -ldflags.
var Version = "dev"

func usage(w io.Writer) {
	fmt.Fprint(w, `agents-room coordinates AI coding agents working in the same codebase.

Usage:
  agents-room join   [--room NAME] [--also SHARED]... [--label TEXT] [--tag T]... [--autonomy 0|1|2] [--key K]
  agents-room leave  --session ID [--room NAME]
  agents-room who    [--room NAME] [--json]
  agents-room cap    [--room NAME] [--set 0|1|2]
  agents-room send   --session ID --to SELECTOR [--subject TEXT] [--body TEXT] [--ttl SECONDS]
  agents-room recv   --session ID [--limit N] [--json]
  agents-room ack    --session ID --message ID
  agents-room lease  claim|renew|release|check|list --session ID [--path PATH] [--ttl SECONDS] [--mode exclusive|shared]
  agents-room drift  note|check --session ID --path PATH
  agents-room watch  [--room NAME] [--interval SECONDS]
  agents-room prune  [--room NAME] [--older-than HOURS]
  agents-room mcp                        serve the room over MCP on stdio
  agents-room hook  --harness H --event E [--autonomy N] [--room NAME]
                                         run one harness hook event
  agents-room update [--check] [--yes]   check GitHub for a newer release
  agents-room migrate-config [--dry-run] rewrite old command names in harness configs
  agents-room doctor                     report what is installed and wired up
  agents-room path

Selectors for --to: * | <session-id> | file:<glob> | tag:<tag>
Autonomy: 0 notify, 1 hold, 2 wake.
`)
}

func open(stderr io.Writer) (*hub.Hub, int) {
	h, err := hub.Open()
	if err != nil {
		fmt.Fprintf(stderr, "open store: %v\n", err)
		return nil, 1
	}
	return h, 0
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "error: %v\n", err)
	return 1
}

func runJoin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", "", "room name (default: repo:<directory>)")
	label := fs.String("label", "", "human label for this session")
	autonomy := fs.Int("autonomy", hub.AutonomyNotify, "0 notify, 1 hold, 2 wake")
	key := fs.String("key", "", "stable harness session key; a repeat join updates the same session")
	harness := fs.String("harness", "", "harness name (default: detect from the environment)")
	var tags, extraRooms stringList
	fs.Var(&tags, "tag", "tag for addressing (repeatable)")
	fs.Var(&extraRooms, "also", "additional shared room to join (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	s, err := h.Join(ctx, hub.JoinOptions{
		Room: *room, ExtraRooms: extraRooms, Label: *label, Tags: tags,
		Autonomy: *autonomy, HarnessKey: *key, Harness: *harness,
	})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, s.ID)
	return 0
}

func runLeave(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id")
	room := fs.String("room", "", "leave only this room; omit to leave every room")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *session == "" {
		return fail(stderr, fmt.Errorf("--session is required"))
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	if *room != "" {
		if err := h.LeaveRoom(ctx, *session, *room); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if err := h.Leave(ctx, *session); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func runWho(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", "", "room name (default: repo:<directory>)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	name := *room
	if name == "" {
		name = hub.RepoRoom("")
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	sessions, err := h.Who(ctx, name)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, sessions)
	}
	if len(sessions) == 0 {
		fmt.Fprintf(stdout, "no sessions in %s\n", name)
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tHARNESS\tAUTONOMY\tROOMS\tLABEL\tTAGS")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%v\n",
			s.ID, s.Harness, s.Autonomy, strings.Join(s.Rooms, ","), s.Label, s.Tags)
	}
	if err := tw.Flush(); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func runCap(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", "", "room name (default: repo:<directory>)")
	set := fs.Int("set", -1, "new cap: 0 notify, 1 hold, 2 wake; omit to read")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	name := *room
	if name == "" {
		name = hub.RepoRoom("")
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	if *set >= 0 {
		cap, err := h.SetRoomAutonomyCap(ctx, name, *set)
		if err != nil {
			return fail(stderr, err)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, map[string]any{"room": name, "autonomy": cap})
		}
		fmt.Fprintf(stdout, "%s autonomy cap: %d\n", name, cap)
		return 0
	}

	cap, err := h.RoomAutonomyCap(ctx, name)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, map[string]any{"room": name, "autonomy": cap})
	}
	fmt.Fprintf(stdout, "%s autonomy cap: %d\n", name, cap)
	return 0
}

func runSend(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "sender session id")
	to := fs.String("to", "*", "selector: * | <session-id> | file:<glob> | tag:<tag>")
	subject := fs.String("subject", "", "subject")
	body := fs.String("body", "", "body")
	ttl := fs.Int64("ttl", 0, "time to live in seconds")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *session == "" {
		return fail(stderr, fmt.Errorf("--session is required"))
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	m, err := h.Send(ctx, hub.SendOptions{
		From: *session, To: *to, Subject: *subject, Body: *body, TTL: *ttl,
	})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, m.ID)
	return 0
}

func runRecv(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id")
	limit := fs.Int("limit", 50, "maximum messages")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *session == "" {
		return fail(stderr, fmt.Errorf("--session is required"))
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	msgs, err := h.Recv(ctx, *session, *limit)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, msgs)
	}
	if len(msgs) == 0 {
		fmt.Fprintln(stdout, "no unread messages")
		return 0
	}
	for _, m := range msgs {
		fmt.Fprintf(stdout, "from %s at %s\nsubject: %s\n%s\n\n",
			m.Sender, time.Unix(m.CreatedAt, 0).Format(time.RFC3339), m.Subject, m.Body)
	}
	return 0
}

func runAck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id")
	message := fs.String("message", "", "message id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *session == "" || *message == "" {
		return fail(stderr, fmt.Errorf("--session and --message are required"))
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()
	if err := h.Ack(ctx, *session, *message); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func runLease(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, fmt.Errorf("lease needs a subcommand: claim, release, check or list"))
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("lease "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id")
	path := fs.String("path", "", "path")
	ttl := fs.Int64("ttl", 1800, "lease lifetime in seconds")
	mode := fs.String("mode", hub.ModeExclusive, "exclusive or shared")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *session == "" {
		return fail(stderr, fmt.Errorf("--session is required"))
	}
	if (sub == "claim" || sub == "release" || sub == "check" || sub == "renew") && *path == "" {
		return fail(stderr, fmt.Errorf("--path is required"))
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	switch sub {
	case "claim":
		res, err := h.Claim(ctx, *session, *path, *ttl, *mode)
		if err != nil {
			return fail(stderr, err)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, res)
		}
		if !res.OK {
			fmt.Fprintf(stdout, "denied: %s holds %s\n", res.Holder.Owner, res.Holder.Path)
			return 1
		}
		fmt.Fprintf(stdout, "granted: %s until %s\n", res.Holder.Path,
			time.Unix(res.Holder.ExpiresAt, 0).Format(time.RFC3339))
		return 0

	case "renew":
		lease, err := h.Renew(ctx, *session, *path, *ttl)
		if err != nil {
			return fail(stderr, err)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, lease)
		}
		fmt.Fprintf(stdout, "renewed: %s until %s\n", lease.Path,
			time.Unix(lease.ExpiresAt, 0).Format(time.RFC3339))
		return 0

	case "release":
		if err := h.Release(ctx, *session, *path); err != nil {
			return fail(stderr, err)
		}
		return 0

	case "check":
		lease, err := h.Check(ctx, *session, *path)
		if err != nil {
			return fail(stderr, err)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, lease)
		}
		if lease == nil {
			fmt.Fprintln(stdout, "free")
			return 0
		}
		fmt.Fprintf(stdout, "%s holds %s until %s\n", lease.Owner, lease.Path,
			time.Unix(lease.ExpiresAt, 0).Format(time.RFC3339))
		return 0

	case "list":
		leases, err := h.List(ctx, *session)
		if err != nil {
			return fail(stderr, err)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, leases)
		}
		if len(leases) == 0 {
			fmt.Fprintln(stdout, "no leases")
			return 0
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PATH\tOWNER\tMODE\tEXPIRES")
		for _, l := range leases {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.Path, l.Owner, l.Mode,
				time.Unix(l.ExpiresAt, 0).Format(time.RFC3339))
		}
		if err := tw.Flush(); err != nil {
			return fail(stderr, err)
		}
		return 0

	default:
		return fail(stderr, fmt.Errorf("unknown lease subcommand %q", sub))
	}
}

func runDrift(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, fmt.Errorf("drift needs a subcommand: note or check"))
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("drift "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id")
	path := fs.String("path", "", "path")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *session == "" || *path == "" {
		return fail(stderr, fmt.Errorf("--session and --path are required"))
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	switch sub {
	case "note":
		sum, err := h.NoteRead(ctx, *session, *path)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, sum)
		return 0
	case "check":
		res, err := h.DriftCheck(ctx, *session, *path)
		if err != nil {
			return fail(stderr, err)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, res)
		}
		fmt.Fprintf(stdout, "%s %s\n", res.Severity, res.Message)
		if res.Severity == hub.DriftCritical {
			return 1
		}
		return 0
	default:
		return fail(stderr, fmt.Errorf("unknown drift subcommand %q", sub))
	}
}

func runPrune(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", "", "room name (default: repo:<directory>)")
	hours := fs.Int("older-than", 24, "remove sessions not seen for this many hours")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := *room
	if name == "" {
		name = hub.RepoRoom("")
	}

	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()

	n, err := h.PruneStale(ctx, name, time.Duration(*hours)*time.Hour)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "removed %d stale session(s) from %s\n", n, name)
	return 0
}

// runMCP serves the room over MCP on stdio. Nothing may be written to
// stdout except the protocol, so this takes no writer.
func runMCP(stdout, stderr io.Writer) int {
	if err := mcpserver.Serve(); err != nil {
		fmt.Fprintf(stderr, "mcp: %v\n", err)
		return 1
	}
	return 0
}

func runPath(stdout, stderr io.Writer) int {
	h, code := open(stderr)
	if code != 0 {
		return code
	}
	defer h.Close()
	fmt.Fprintln(stdout, h.Path())
	return 0
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return fmt.Sprint([]string(*s)) }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Main is the entry point used by cmd/agents-room.
func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}
