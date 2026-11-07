package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/asovetov/agents-room/internal/hub"
)

// runWatch prints the room state until interrupted. It polls, because there
// is no daemon to subscribe to; the interval is the operator's choice.
func runWatch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", "", "room name (default: repo:<directory>)")
	interval := fs.Int("interval", 2, "seconds between refreshes")
	once := fs.Bool("once", false, "print one snapshot and exit")
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

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	render := func() {
		sessions, err := h.Who(ctx, name)
		if err != nil {
			fmt.Fprintf(stderr, "who: %v\n", err)
			return
		}
		fmt.Fprintf(stdout, "room %s  %s\n\n", name, time.Now().Format("15:04:05"))
		if len(sessions) == 0 {
			fmt.Fprintln(stdout, "  no sessions")
			return
		}
		for _, s := range sessions {
			age := time.Since(time.Unix(s.LastSeen, 0)).Round(time.Second)
			label := s.Label
			if label == "" {
				label = s.ID
			}
			fmt.Fprintf(stdout, "  %-10s %-12s %-7s autonomy=%d  seen %s ago\n",
				label, s.Harness, livenessWord(s.Liveness), s.Autonomy, age)
			leases, err := h.List(ctx, s.ID)
			if err != nil {
				continue
			}
			for _, l := range leases {
				if l.Owner == s.ID {
					fmt.Fprintf(stdout, "    holds %s\n", l.Path)
				}
			}
			if n, err := h.Unread(ctx, s.ID); err == nil && n > 0 {
				fmt.Fprintf(stdout, "    %d unread\n", n)
			}
		}
	}

	if *once {
		render()
		return 0
	}

	ticker := time.NewTicker(time.Duration(*interval) * time.Second)
	defer ticker.Stop()
	for {
		render()
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}
