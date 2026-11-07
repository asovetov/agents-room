package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/asovetov/agents-room/internal/hub"
)

// runHistory prints what has been said in a room, newest last.
//
// It is the command a session runs after joining a room where work has been
// going on. Recv answers "what is waiting for me" and correctly returns
// nothing to a newcomer; this answers "what has been decided here".
func runHistory(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", "", "room name (default: repo:<directory>)")
	limit := fs.Int("limit", 20, "maximum messages")
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

	entries, err := h.History(ctx, name, *limit)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		return writeJSON(stdout, stderr, entries)
	}
	if len(entries) == 0 {
		fmt.Fprintf(stdout, "no messages in %s\n", name)
		return 0
	}
	for _, e := range entries {
		from := e.Sender
		if e.SenderTag != "" {
			from = e.SenderTag
		}
		reply := ""
		if e.ReplyTo != "" {
			reply = fmt.Sprintf(" (reply to %s)", e.ReplyTo)
		}
		fmt.Fprintf(stdout, "%s  %s → %s%s\n  %s\n",
			time.Unix(e.CreatedAt, 0).Format("2006-01-02 15:04"), from, e.Selector, reply, e.Subject)
		if e.Body != "" {
			fmt.Fprintf(stdout, "  %s\n", e.Body)
		}
		fmt.Fprintln(stdout)
	}
	return 0
}
