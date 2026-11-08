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
	case "sent":
		return runSent(ctx, rest, stdout, stderr)
	case "history":
		return runHistory(ctx, rest, stdout, stderr)
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
	fmt.Fprint(w, `agents-room coordinates AI coding agents across repositories:
sessions in different checkouts meet in a shared room to agree on a contract.

Usage:
  agents-room join   [--room NAME] [--also SHARED]... [--label TEXT] [--tag T]... [--autonomy 0|1|2] [--key K]
  agents-room leave  --session ID [--room NAME]
  agents-room who    [--room NAME] [--json]
  agents-room cap    [--room NAME] [--set 0|1|2]
  agents-room send   --session ID --to SELECTOR [--subject TEXT] [--body TEXT] [--ttl SECONDS]
  agents-room recv   --session ID [--limit N] [--json]
  agents-room sent   --session ID [--limit N] [--json]
  agents-room history [--room NAME] [--limit N] [--json]
                                         what has been said in a room
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

	// Only an explicit --autonomy asks for a level. Without one, a rejoin
	// keeps what the session already has, so running `agents-room join` by
	// hand does not silently lower a chat the hook had configured.
	opt := hub.JoinOptions{
		Room: *room, ExtraRooms: extraRooms, Label: *label, Tags: tags,
		HarnessKey: *key, Harness: *harness,
	}
	if autonomySet(args) {
		opt.Autonomy, opt.AutonomySet = *autonomy, true
	}
	s, err := h.Join(ctx, opt)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, s.ID)
	if s.Warning != "" {
		// The id still goes to stdout: the join worked, and a caller parsing
		// it must not have to cope with a warning in the middle of the line.
		fmt.Fprintf(stderr, "warning: %s\n", s.Warning)
	}
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
	// Liveness is a column of its own because last_seen cannot stand in for
	// it: a chat thinking through a long edit and a chat that exited an hour
	// ago look the same in "seen 60m ago", and the difference decides whether
	// a file is really held.
	fmt.Fprintln(tw, "SESSION\tSTATE\tHARNESS\tAUTONOMY\tROOMS\tLABEL\tTAGS")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%v\n",
			s.ID, livenessWord(s.Liveness), s.Harness, s.Autonomy,
			strings.Join(s.Rooms, ","), s.Label, s.Tags)
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
	// The warning explains why a file: selector reached nobody, which is
	// usually a path in another repository. A bare "matched no recipient"
	// leaves the sender with no idea what to change.
	if m.Warning != "" {
		fmt.Fprintf(stderr, "warning: %s\n", m.Warning)
	} else if m.Recipients == 0 {
		fmt.Fprintf(stderr, "warning: %s matched no recipient; the message is stored but nobody will read it\n", m.Selector)
	}
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
		// "no unread messages" alone cannot be told from "you are looking in
		// the wrong place", and an agent that suspects the second has no way
		// to check short of reading the database.
		inbox, err := h.Inbox(ctx, *session)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, describeInbox(inbox))
		return 0
	}
	for _, m := range msgs {
		fmt.Fprintf(stdout, "from %s at %s\nsubject: %s\n%s\n\n",
			m.Sender, time.Unix(m.CreatedAt, 0).Format(time.RFC3339), m.Subject, m.Body)
	}
	return 0
}

func runSent(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id")
	limit := fs.Int("limit", 20, "maximum messages")
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

	sent, err := h.SentMessages(ctx, *session, *limit)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, sent)
	}
	if len(sent) == 0 {
		fmt.Fprintln(stdout, "nothing sent from this session")
		return 0
	}
	for _, s := range sent {
		fmt.Fprintf(stdout, "%s  %s\n  to %s at %s: %s\n",
			s.ID, s.Subject, s.To,
			time.Unix(s.CreatedAt, 0).Format(time.RFC3339),
			describeStates(s.States))
		if s.Pending > 0 {
			fmt.Fprintf(stdout, "  %d of %d recipient(s) have not read it yet\n",
				s.Pending, s.Sent)
		}
		// A recipient that left is not one that is still waiting. Saying so
		// separately is the difference between "keep watching" and "this one
		// is never going to answer".
		if s.Left > 0 {
			fmt.Fprintf(stdout, "  %d recipient(s) left the room before reading it\n", s.Left)
		}
		// Who read it, not just how many. In a shared room a broadcast reaches
		// everyone, and "2 read it" says nothing about whether either of them
		// was the agent the message was about.
		for _, r := range s.Readers {
			fmt.Fprintf(stdout, "  read by %s", readerName(r))
			if len(r.Rooms) > 0 {
				fmt.Fprintf(stdout, " (%s)", strings.Join(r.Rooms, ", "))
			}
			if r.SharedOnly {
				fmt.Fprintf(stdout, " — meets you only in a shared room")
			}
			fmt.Fprintln(stdout)
		}
	}
	return 0
}

// autonomySet reports whether --autonomy appeared on the command line.
//
// The flag's default is 0, which is a real level, so the value alone cannot
// say whether the caller asked for notify or said nothing at all. The
// difference matters on a rejoin: one keeps the stored level, the other
// lowers it.
func autonomySet(args []string) bool {
	for _, a := range args {
		if a == "--autonomy" || a == "-autonomy" || strings.HasPrefix(a, "--autonomy=") ||
			strings.HasPrefix(a, "-autonomy=") {
			return true
		}
	}
	return false
}

// readerName is what a human calls a reader: its label when it has one, its
// id otherwise.
func readerName(r hub.Reader) string {
	if r.Label != "" {
		return r.Label
	}
	return r.SessionID
}

// describeInbox turns an empty recv into an answer the reader can act on: the
// rooms the session is in, and whether anything has ever reached it.
func describeInbox(inbox *hub.InboxSummary) string {
	var b strings.Builder
	b.WriteString("no unread messages")
	if len(inbox.Rooms) > 0 {
		fmt.Fprintf(&b, "; this session is in %s", strings.Join(inbox.Rooms, ", "))
	}
	switch {
	case inbox.Delivered == 0:
		b.WriteString(". Nothing has ever been sent to it: if you expected a " +
			"message, check that the sender and you share a room")
	case inbox.Read == inbox.Delivered:
		fmt.Fprintf(&b, ". %d message(s) were delivered and all of them read",
			inbox.Delivered)
	default:
		fmt.Fprintf(&b, ". %d of %d delivered message(s) have been read",
			inbox.Read, inbox.Delivered)
	}
	return b.String()
}

// livenessWord renders a session's liveness for the who table. "unknown" is
// spelled out rather than left blank: an empty cell reads as "alive enough",
// and the whole point is that nobody knows.
func livenessWord(liveness string) string {
	if liveness == "" {
		return hub.LivenessUnknown
	}
	return liveness
}

// describeStates renders a delivery-state tally in a fixed order, so two runs
// of the same data read the same way.
func describeStates(states map[string]int) string {
	if len(states) == 0 {
		return "no recipients"
	}
	order := []string{
		hub.StateStored, hub.StateOffered, hub.StateAccepted,
		hub.StateRead, hub.StateAcked, hub.StateExpired, hub.StateUndeliverable,
	}
	var parts []string
	for _, state := range order {
		if n := states[state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, state))
		}
	}
	return strings.Join(parts, ", ")
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
		if res.Warning != "" {
			fmt.Fprintf(stderr, "warning: %s\n", res.Warning)
		}
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
