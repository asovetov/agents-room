// Package mcpserver exposes agents-room as MCP tools.
//
// The server is stateless in the MCP sense: room_join returns a session id
// and every other tool takes it back as an explicit handle. That is the shape
// the MCP 2026-07-28 revision asks for, and it means the server can be
// restarted without losing anything that matters.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/asovetov/agents-room/internal/hub"
)

// Version is set at build time with -ldflags.
var Version = "dev"

// New builds the MCP server with every agents-room tool registered.
func New(h *hub.Hub) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "agents-room",
		Version: Version,
	}, nil)

	registerRoomTools(s, h)
	registerMessageTools(s, h)
	registerLeaseTools(s, h)
	registerDriftTools(s, h)

	// A session that calls a tool is alive, and room_who has to say so.
	// Every tool already carries session_id, so one hook covers all of
	// them without a Touch call in each handler.
	s.AddReceivingMiddleware(touchOnToolCall(h))

	return s
}

// touchOnToolCall updates last_seen for the session named in a tools/call
// argument. A missing or unknown session id is not an error: the tool
// handler reports that itself, with a better message.
func touchOnToolCall(h *hub.Hub) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
					var args struct {
						SessionID string `json:"session_id"`
					}
					if json.Unmarshal(params.Arguments, &args) == nil && args.SessionID != "" {
						_ = h.Touch(ctx, args.SessionID)
					}
				}
			}
			return next(ctx, method, req)
		}
	}
}

// ---- room tools ----

type joinIn struct {
	Room       string   `json:"room,omitempty" jsonschema:"repository room; omit for repo:<current directory>"`
	ExtraRooms []string `json:"extra_rooms,omitempty" jsonschema:"shared rooms to join as well, e.g. storefront"`
	Label      string   `json:"label,omitempty" jsonschema:"short human label for this session"`
	Tags       []string `json:"tags,omitempty" jsonschema:"tags used by tag: selectors"`
	// A pointer, so "omitted" and "0" stay different things. With a plain
	// int, a room_join that never mentioned autonomy arrived as 0 and wrote
	// notify over the level the SessionStart hook had set, after which the
	// chat was never woken while idle.
	Autonomy   *int   `json:"autonomy,omitempty" jsonschema:"0 notify, 1 hold, 2 wake; omit to keep the level you already have"`
	Harness    string `json:"harness,omitempty" jsonschema:"harness name; omit to detect from the environment"`
	HarnessKey string `json:"harness_key,omitempty" jsonschema:"stable session id from the harness; makes a rejoin update the same session"`
}

type joinOut struct {
	SessionID string   `json:"session_id"`
	Rooms     []string `json:"rooms"`
	Autonomy  int      `json:"autonomy"`
	Warning   string   `json:"warning,omitempty"`
}

func registerRoomTools(s *mcp.Server, h *hub.Hub) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "room_join",
		Description: "Join a room and get a session id. Call this once at the start. " +
			"Every other tool takes the returned session_id. The room defaults to " +
			"repo:<name of the current git repository>. Pass extra_rooms to also " +
			"join shared rooms, which is how an agent in one repository reaches an " +
			"agent in another.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, joinOut, error) {
		// The server is spawned by the harness, so it inherits the harness's
		// own session id. Using it when the model omits harness_key keeps a
		// chat that calls room_join twice from becoming two sessions, which
		// is exactly what happened before.
		key := in.HarnessKey
		if key == "" {
			key = hub.HarnessKeyFromEnv()
		}
		opt := hub.JoinOptions{
			Room: in.Room, ExtraRooms: in.ExtraRooms, Label: in.Label,
			Tags: in.Tags, Harness: in.Harness, HarnessKey: key,
		}
		if in.Autonomy != nil {
			opt.Autonomy, opt.AutonomySet = *in.Autonomy, true
		}
		sess, err := h.Join(ctx, opt)
		if err != nil {
			return nil, joinOut{}, err
		}
		msg := fmt.Sprintf("joined %s as %s", strings.Join(sess.Rooms, ", "), sess.ID)
		if sess.Warning != "" {
			// The model reads this text, and a wrong key silently adopts
			// another chat's session, so it has to be visible, not logged.
			msg += "\nwarning: " + sess.Warning
		}
		// A session joining a room where work is already under way has no
		// unread deliveries, so msg_recv will correctly say nothing is
		// waiting -- and the model then assumes nothing has happened. One
		// line here is the difference between an agent that reads the room's
		// history and one that starts from zero.
		if summary := roomActivity(ctx, h, sess.Rooms); summary != "" {
			msg += "\n" + summary
		}
		return text(msg),
			joinOut{SessionID: sess.ID, Rooms: sess.Rooms, Autonomy: sess.Autonomy, Warning: sess.Warning}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "room_set_autonomy_cap",
		Description: "Operator control: set the highest autonomy any session may reach in " +
			"a room. Lowering it quiets the whole room without touching each session. " +
			"Sessions already running keep their current level until they rejoin.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in capIn) (*mcp.CallToolResult, capOut, error) {
		room, err := resolveRoom(ctx, h, in.Room, in.SessionID)
		if err != nil {
			return nil, capOut{}, err
		}
		cap, err := h.SetRoomAutonomyCap(ctx, room, in.Autonomy)
		if err != nil {
			return nil, capOut{}, err
		}
		return text(fmt.Sprintf("autonomy cap for %s is now %d", room, cap)),
			capOut{Room: room, Autonomy: cap}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "room_leave",
		Description: "Leave every room and release every lease this session holds. " +
			"To leave one shared room and stay in the rest, use room_leave_room.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, emptyOut, error) {
		if err := h.Leave(ctx, in.SessionID); err != nil {
			return nil, emptyOut{}, err
		}
		return text("left every room"), emptyOut{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "room_leave_room",
		Description: "Leave one room and stay in the others. Leases held in that " +
			"room are released. Use it when a shared-room contract is settled.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in leaveRoomIn) (*mcp.CallToolResult, emptyOut, error) {
		if err := h.LeaveRoom(ctx, in.SessionID, in.Room); err != nil {
			return nil, emptyOut{}, err
		}
		return text("left " + in.Room), emptyOut{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "room_who",
		Description: "List the sessions in a room with their harness, label, tags and " +
			"effective autonomy. Use this to find out who else is working here before " +
			"starting on shared files.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in whoIn) (*mcp.CallToolResult, whoOut, error) {
		room, err := resolveRoom(ctx, h, in.Room, in.SessionID)
		if err != nil {
			return nil, whoOut{}, err
		}
		sessions, err := h.Who(ctx, room)
		if err != nil {
			return nil, whoOut{}, err
		}
		return text(fmt.Sprintf("%d session(s) in %s", len(sessions), room)),
			whoOut{Room: room, Sessions: sessions}, nil
	})
}

// resolveRoom picks the room a tool should act on: the one named, else the
// caller's own, else the current directory's.
//
// A named session that does not exist is an error rather than a fall-through
// to the current directory. The fall-through is how a wrong id did the wrong
// thing quietly: `room_set_autonomy_cap` with a mistyped session id wrote the
// cap into whatever repository the MCP server happened to be running in and
// reported success.
func resolveRoom(ctx context.Context, h *hub.Hub, room, sessionID string) (string, error) {
	if room != "" {
		return room, nil
	}
	if sessionID != "" {
		return h.RoomFor(ctx, sessionID)
	}
	return hub.RepoRoom(""), nil
}

type sessionIn struct {
	SessionID string `json:"session_id" jsonschema:"session id returned by room_join"`
}

type leaveRoomIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Room      string `json:"room" jsonschema:"room to leave"`
}

type whoIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"your session id; used to find your room"`
	Room      string `json:"room,omitempty" jsonschema:"room name; overrides the session room"`
}

type whoOut struct {
	Room     string        `json:"room"`
	Sessions []hub.Session `json:"sessions"`
}

type capIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"your session id; used to find your room"`
	Room      string `json:"room,omitempty" jsonschema:"room name; overrides the session room"`
	Autonomy  int    `json:"autonomy" jsonschema:"highest autonomy allowed: 0 notify, 1 hold, 2 wake"`
}

type capOut struct {
	Room     string `json:"room"`
	Autonomy int    `json:"autonomy"`
}

type emptyOut struct{}

// ---- message tools ----

type sendIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	To        string `json:"to" jsonschema:"* | <session-id> | file:<glob> | file:<repo>:<path> | tag:<tag>"`
	Subject   string `json:"subject,omitempty" jsonschema:"short subject"`
	Body      string `json:"body,omitempty" jsonschema:"message body"`
	ReplyTo   string `json:"reply_to,omitempty" jsonschema:"id of the message being answered"`
	TTL       int64  `json:"ttl_s,omitempty" jsonschema:"seconds before the message expires; 0 means never"`
}

type sendOut struct {
	MessageID string `json:"message_id"`
	// Warning is set when the message was stored but looks like it reached
	// the wrong place, such as a file: selector that matched nobody.
	Warning string `json:"warning,omitempty"`
	// Recipients is zero when the selector matched nobody: the message is
	// stored but will never be read, so the sender must not assume delivery.
	Recipients int `json:"recipients"`
}

func registerMessageTools(s *mcp.Server, h *hub.Hub) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "msg_send",
		Description: "Send a message to a selector. '*': everyone in the room except you. " +
			"'file:<glob>': whoever holds a lease matching the glob, which is how you reach " +
			"the agent editing a file without knowing its session id. 'file:<repo>:<path>': " +
			"the same, in another repository — the path is read from that repository's " +
			"checkout, so mobile can write file:api:src/users.go for api's file. 'tag:<tag>': " +
			"everyone who joined with that tag. Delivery is asynchronous: the recipient is " +
			"nudged and fetches the body with msg_recv.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, sendOut, error) {
		m, err := h.Send(ctx, hub.SendOptions{
			From: in.SessionID, To: in.To, Subject: in.Subject,
			Body: in.Body, ReplyTo: in.ReplyTo, TTL: in.TTL,
		})
		if err != nil {
			return nil, sendOut{}, err
		}
		if m.Recipients == 0 {
			msg := fmt.Sprintf("stored %s, but %s matched no recipient", m.ID, m.Selector)
			if m.Warning != "" {
				msg += "\n" + m.Warning
			}
			return text(msg), sendOut{MessageID: m.ID, Warning: m.Warning}, nil
		}
		msg := fmt.Sprintf("sent %s to %d session(s)", m.ID, m.Recipients)
		if m.Warning != "" {
			msg += "\n" + m.Warning
		}
		return text(msg), sendOut{MessageID: m.ID, Recipients: m.Recipients, Warning: m.Warning}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "msg_recv",
		Description: "Fetch unread messages addressed to this session and mark them read. " +
			"Call it when you are nudged, and before starting work if you have been away.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recvIn) (*mcp.CallToolResult, recvOut, error) {
		msgs, err := h.Recv(ctx, in.SessionID, in.Limit)
		if err != nil {
			return nil, recvOut{}, err
		}
		if len(msgs) > 0 {
			return text(fmt.Sprintf("%d message(s)", len(msgs))), recvOut{Messages: msgs}, nil
		}
		// An empty inbox is the answer most likely to be misunderstood, so it
		// carries the two facts that disambiguate it: where this session can
		// be reached, and whether anything has ever reached it.
		inbox, err := h.Inbox(ctx, in.SessionID)
		if err != nil {
			return nil, recvOut{}, err
		}
		return text(describeEmptyInbox(inbox)), recvOut{Messages: msgs, Inbox: inbox}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "msg_ack",
		Description: "Confirm you acted on a message. Ack is the last delivery state.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ackIn) (*mcp.CallToolResult, emptyOut, error) {
		if err := h.Ack(ctx, in.SessionID, in.MessageID); err != nil {
			return nil, emptyOut{}, err
		}
		return text("acked"), emptyOut{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "msg_unread",
		Description: "Count unread messages. Cheap; use it to decide whether to call msg_recv " +
			"without pulling bodies into context.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, unreadOut, error) {
		n, err := h.Unread(ctx, in.SessionID)
		if err != nil {
			return nil, unreadOut{}, err
		}
		return text(fmt.Sprintf("%d unread", n)), unreadOut{Count: n}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "msg_sent",
		Description: "List what this session sent and what became of it: how many " +
			"recipients have read it, and how many have not. Use it when a reply " +
			"never arrived, to tell a slow reader from a message nobody picked up.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sentIn) (*mcp.CallToolResult, sentOut, error) {
		sent, err := h.SentMessages(ctx, in.SessionID, in.Limit)
		if err != nil {
			return nil, sentOut{}, err
		}
		return text(describeSent(sent)), sentOut{Messages: sent}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "room_history",
		Description: "Read what has been said in a room recently. Call this after " +
			"joining a room where work is already under way: msg_recv only shows " +
			"messages addressed to you since you joined, so it says nothing about " +
			"what the others already agreed. Read-only; it does not mark anything read.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyOut, error) {
		room, err := resolveRoom(ctx, h, in.Room, in.SessionID)
		if err != nil {
			return nil, historyOut{}, err
		}
		if room == "" {
			room = hub.RepoRoom("")
		}
		entries, err := h.History(ctx, room, in.Limit)
		if err != nil {
			return nil, historyOut{}, err
		}
		return text(describeHistory(room, entries)), historyOut{Room: room, Messages: entries}, nil
	})
}

// describeHistory renders a room's recent messages as the lines a model reads.
func describeHistory(room string, entries []hub.HistoryEntry) string {
	if len(entries) == 0 {
		return fmt.Sprintf("no messages in %s", room)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d message(s) in %s, oldest first:\n", len(entries), room)
	for _, e := range entries {
		from := e.Sender
		if e.SenderTag != "" {
			from = e.SenderTag
		}
		fmt.Fprintf(&b, "%s  %s -> %s: %s\n",
			time.Unix(e.CreatedAt, 0).Format("01-02 15:04"), from, e.Selector, e.Subject)
		if e.Body != "" {
			fmt.Fprintf(&b, "    %s\n", e.Body)
		}
	}
	return b.String()
}

// describeEmptyInbox turns "nothing waiting" into an answer a model can act on.
//
// The two failure modes it separates: a session that is in the wrong room
// (nothing was ever addressed to it) and a session that has caught up (it was
// addressed and read it all). Both are an empty Recv, and only the second one
// means "nothing more to do".
func describeEmptyInbox(inbox *hub.InboxSummary) string {
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

// describeSent renders the sent list as the few lines a model reads.
func describeSent(sent []hub.Sent) string {
	if len(sent) == 0 {
		return "nothing sent from this session"
	}
	var b strings.Builder
	for _, s := range sent {
		fmt.Fprintf(&b, "%s  %s\n  to %s: %s\n", s.ID, s.Subject, s.To, statesLine(s.States))
		if s.Pending > 0 {
			fmt.Fprintf(&b, "  %d recipient(s) have not read it yet\n", s.Pending)
		}
		// Who, not just how many. A broadcast in a shared room reaches every
		// member, and a count cannot say whether any of them was the agent the
		// message was about.
		for _, r := range s.Readers {
			who := r.Label
			if who == "" {
				who = r.SessionID
			}
			fmt.Fprintf(&b, "  read by %s", who)
			if len(r.Rooms) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(r.Rooms, ", "))
			}
			if r.SharedOnly {
				b.WriteString(" — meets you only in a shared room")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// statesLine renders a delivery-state tally in a fixed order.
func statesLine(states map[string]int) string {
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

type recvIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum messages to fetch"`
}

type recvOut struct {
	Messages []hub.Message `json:"messages"`
	// Inbox explains an empty answer: which rooms the session is in and
	// whether anything was ever delivered to it. Without it "0 message(s)"
	// cannot be told from "you are looking in the wrong place".
	Inbox *hub.InboxSummary `json:"inbox,omitempty"`
}

type ackIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	MessageID string `json:"message_id" jsonschema:"message to confirm"`
}

type unreadOut struct {
	Count int `json:"count"`
}

type sentIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum messages to list, newest first"`
}

type sentOut struct {
	Messages []hub.Sent `json:"messages"`
}

// ---- lease tools ----

type claimIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Path      string `json:"path" jsonschema:"file or directory you are about to change"`
	TTL       int64  `json:"ttl_s,omitempty" jsonschema:"seconds the lease stays valid; default 1800"`
	Mode      string `json:"mode,omitempty" jsonschema:"exclusive (default) or shared"`
}

type renewIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Path      string `json:"path" jsonschema:"path you already hold"`
	TTL       int64  `json:"ttl_s,omitempty" jsonschema:"seconds the lease stays valid from now; default 1800"`
}

type releaseIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Path      string `json:"path" jsonschema:"path to release"`
}

type checkIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Path      string `json:"path" jsonschema:"path to check"`
}

type checkOut struct {
	Free  bool       `json:"free"`
	Lease *hub.Lease `json:"lease,omitempty"`
}

type listOut struct {
	Leases []hub.Lease `json:"leases"`
}

func registerLeaseTools(s *mcp.Server, h *hub.Hub) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "lease_claim",
		Description: "Claim a path before editing it. If another session holds an exclusive " +
			"lease you get ok=false with the holder, and you should talk to them or wait. " +
			"Leases expire, so a crashed session cannot block a file forever.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in claimIn) (*mcp.CallToolResult, hub.LeaseResult, error) {
		res, err := h.Claim(ctx, in.SessionID, in.Path, in.TTL, in.Mode)
		if err != nil {
			return nil, hub.LeaseResult{}, err
		}
		if !res.OK {
			return text(fmt.Sprintf("denied: %s holds %s", res.Holder.Owner, res.Holder.Path)), *res, nil
		}
		msg := "granted: " + res.Holder.Path
		if res.Warning != "" {
			msg += "\n" + res.Warning
		}
		return text(msg), *res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "lease_renew",
		Description: "Extend a lease you already hold. Use it when an edit outlives the " +
			"TTL. It never takes a path you do not hold, so a stale agent cannot " +
			"re-acquire a file someone else has since claimed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in renewIn) (*mcp.CallToolResult, hub.Lease, error) {
		lease, err := h.Renew(ctx, in.SessionID, in.Path, in.TTL)
		if err != nil {
			return nil, hub.Lease{}, err
		}
		return text("renewed: " + lease.Path), *lease, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "lease_release",
		Description: "Release a lease when you are done with a path.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in releaseIn) (*mcp.CallToolResult, emptyOut, error) {
		if err := h.Release(ctx, in.SessionID, in.Path); err != nil {
			return nil, emptyOut{}, err
		}
		return text("released"), emptyOut{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "lease_check",
		Description: "Ask who holds a path right now, before reading or editing it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in checkIn) (*mcp.CallToolResult, checkOut, error) {
		lease, err := h.Check(ctx, in.SessionID, in.Path)
		if err != nil {
			return nil, checkOut{}, err
		}
		if lease == nil {
			return text("free"), checkOut{Free: true}, nil
		}
		return text(lease.Owner + " holds " + lease.Path), checkOut{Lease: lease}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "lease_list",
		Description: "List every live lease in the room.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, listOut, error) {
		leases, err := h.List(ctx, in.SessionID)
		if err != nil {
			return nil, listOut{}, err
		}
		return text(fmt.Sprintf("%d lease(s)", len(leases))), listOut{Leases: leases}, nil
	})
}

// ---- drift tools ----

type noteReadIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Path      string `json:"path" jsonschema:"file you just read"`
}

type noteReadOut struct {
	Hash string `json:"hash"`
}

type driftIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Path      string `json:"path" jsonschema:"file to check"`
}

func registerDriftTools(s *mcp.Server, h *hub.Hub) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "note_read",
		Description: "Record that you read a file, so a later drift_check can tell whether " +
			"it changed under you. Call it after reading a file you intend to edit.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in noteReadIn) (*mcp.CallToolResult, noteReadOut, error) {
		sum, err := h.NoteRead(ctx, in.SessionID, in.Path)
		if err != nil {
			return nil, noteReadOut{}, err
		}
		return text("recorded"), noteReadOut{Hash: sum}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "drift_check",
		Description: "Check whether a file changed since you read it. SAFE: unchanged. " +
			"MINOR: changed, nobody holds a lease. CRITICAL: changed while another session " +
			"held a lease, so re-read before editing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in driftIn) (*mcp.CallToolResult, hub.DriftResult, error) {
		res, err := h.DriftCheck(ctx, in.SessionID, in.Path)
		if err != nil {
			return nil, hub.DriftResult{}, err
		}
		return text(res.Severity + " " + res.Message), *res, nil
	})
}

// text builds a short text result. The structured output carries the detail.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

type historyIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"your session id; used to pick your room when room is omitted"`
	Room      string `json:"room,omitempty" jsonschema:"room to read; omit for your repository room"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum messages, newest kept"`
}

type historyOut struct {
	Room     string             `json:"room"`
	Messages []hub.HistoryEntry `json:"messages"`
}

// roomActivity describes recent traffic in the rooms a session just joined, so
// a joining agent knows whether there is history worth reading.
//
// It reports only rooms that saw something in the last day, and only one line
// each: the point is to prompt a room_history call, not to put the history
// itself into the context.
func roomActivity(ctx context.Context, h *hub.Hub, rooms []string) string {
	var lines []string
	for _, room := range rooms {
		s, err := h.Summarize(ctx, room, 24*time.Hour)
		if err != nil || s == nil || s.Messages == 0 {
			continue
		}
		line := fmt.Sprintf("%s: %d message(s) in the last day", room, s.Messages)
		if s.Last != "" {
			line += fmt.Sprintf(", the last from %s about %q", s.LastFrom, s.Last)
		}
		lines = append(lines, line)
		// Only one line: the room the session came for is the first one it
		// joined, and a wall of summaries is the context bloat this avoids.
		break
	}
	if len(lines) == 0 {
		return ""
	}
	return lines[0] + " -- call room_history to read it"
}
