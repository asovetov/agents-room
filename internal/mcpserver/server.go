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
	Autonomy   int      `json:"autonomy,omitempty" jsonschema:"0 notify, 1 hold, 2 wake"`
	Harness    string   `json:"harness,omitempty" jsonschema:"harness name; omit to detect from the environment"`
	HarnessKey string   `json:"harness_key,omitempty" jsonschema:"stable session id from the harness; makes a rejoin update the same session"`
}

type joinOut struct {
	SessionID string   `json:"session_id"`
	Rooms     []string `json:"rooms"`
	Autonomy  int      `json:"autonomy"`
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
		sess, err := h.Join(ctx, hub.JoinOptions{
			Room: in.Room, ExtraRooms: in.ExtraRooms, Label: in.Label,
			Tags: in.Tags, Autonomy: in.Autonomy,
			Harness: in.Harness, HarnessKey: key,
		})
		if err != nil {
			return nil, joinOut{}, err
		}
		return text(fmt.Sprintf("joined %s as %s", strings.Join(sess.Rooms, ", "), sess.ID)),
			joinOut{SessionID: sess.ID, Rooms: sess.Rooms, Autonomy: sess.Autonomy}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "room_set_autonomy_cap",
		Description: "Operator control: set the highest autonomy any session may reach in " +
			"a room. Lowering it quiets the whole room without touching each session. " +
			"Sessions already running keep their current level until they rejoin.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in capIn) (*mcp.CallToolResult, capOut, error) {
		room := in.Room
		if room == "" && in.SessionID != "" {
			room = h.RoomOf(ctx, in.SessionID)
		}
		if room == "" {
			room = hub.RepoRoom("")
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
		room := in.Room
		if room == "" && in.SessionID != "" {
			room = h.RoomOf(ctx, in.SessionID)
		}
		if room == "" {
			room = hub.RepoRoom("")
		}
		sessions, err := h.Who(ctx, room)
		if err != nil {
			return nil, whoOut{}, err
		}
		return text(fmt.Sprintf("%d session(s) in %s", len(sessions), room)),
			whoOut{Room: room, Sessions: sessions}, nil
	})
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
	To        string `json:"to" jsonschema:"* | <session-id> | file:<glob> | tag:<tag>"`
	Subject   string `json:"subject,omitempty" jsonschema:"short subject"`
	Body      string `json:"body,omitempty" jsonschema:"message body"`
	ReplyTo   string `json:"reply_to,omitempty" jsonschema:"id of the message being answered"`
	TTL       int64  `json:"ttl_s,omitempty" jsonschema:"seconds before the message expires; 0 means never"`
}

type sendOut struct {
	MessageID string `json:"message_id"`
	// Recipients is zero when the selector matched nobody: the message is
	// stored but will never be read, so the sender must not assume delivery.
	Recipients int `json:"recipients"`
}

func registerMessageTools(s *mcp.Server, h *hub.Hub) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "msg_send",
		Description: "Send a message to a selector. '*': everyone in the room except you. " +
			"'file:<glob>': whoever holds a lease matching the glob, which is how you reach " +
			"the agent editing a file without knowing its session id. 'tag:<tag>': everyone " +
			"who joined with that tag. Delivery is asynchronous: the recipient is nudged and " +
			"fetches the body with msg_recv.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, sendOut, error) {
		m, err := h.Send(ctx, hub.SendOptions{
			From: in.SessionID, To: in.To, Subject: in.Subject,
			Body: in.Body, ReplyTo: in.ReplyTo, TTL: in.TTL,
		})
		if err != nil {
			return nil, sendOut{}, err
		}
		if m.Recipients == 0 {
			return text(fmt.Sprintf("stored %s, but %s matched no recipient", m.ID, m.Selector)),
				sendOut{MessageID: m.ID}, nil
		}
		return text(fmt.Sprintf("sent %s to %d session(s)", m.ID, m.Recipients)),
			sendOut{MessageID: m.ID, Recipients: m.Recipients}, nil
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
		return text(fmt.Sprintf("%d message(s)", len(msgs))), recvOut{Messages: msgs}, nil
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
}

type recvIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum messages to fetch"`
}

type recvOut struct {
	Messages []hub.Message `json:"messages"`
}

type ackIn struct {
	SessionID string `json:"session_id" jsonschema:"your session id"`
	MessageID string `json:"message_id" jsonschema:"message to confirm"`
}

type unreadOut struct {
	Count int `json:"count"`
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
		return text("granted: " + res.Holder.Path), *res, nil
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
