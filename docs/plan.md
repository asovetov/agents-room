# agents-room implementation plan

Status: approved 2026-11-06. Milestones M0 to M3 are implemented; see the
Status section at the end.

## What it is

A coordination layer for AI coding agents that work in different repositories
and have to agree with each other. The motivating case is a contract between
repositories: an API in one, its clients in others, each with its own agent
sessions and often its own harness, with nobody to carry a request from one
window to the next.

It answers three questions that no harness answers today:

1. **Who is working on this interface?** Which sessions are in the room, from
   which repository, and which of them are still running.
2. **Who is touching this file?** Leases on paths, with expiry and
   stale-holder detection, across repositories rather than within one.
3. **Is my read still valid?** Drift warnings when a file changed under you.

It also delivers messages addressed by *what* rather than *who*:
`file:src/api/users.go` reaches whoever holds that file, in whatever
repository; `tag:mobile` reaches everyone working on the mobile client.

The single-repository case, several sessions on one codebase, works the same
way and comes for free.

## What it is not

- Not a chat platform. Lasting decisions belong in the operator's long-term
  memory tool.
- Not a replacement for a harness's own cross-session messaging. Where the
  harness can wake a peer natively, agents-room adds leases, presence and
  addressing by file rather than duplicating the transport.
- Not a multi-machine service. Same machine only.
- Not a session authority. Sessions are explicit handles, per MCP 2026-07-28.

## Key design decision: no daemon

The obvious design is a daemon on `127.0.0.1` holding queues in memory. We
deliberately do not do that.

Instead the shared state is a **SQLite database in WAL mode** at
`~/.agents-room/room.db`, and every participant is a short-lived process that
opens it directly:

| Participant | Lifetime | Role |
|---|---|---|
| `agents-room mcp` | one per agent session, stdio | MCP tools, holds the session handle |
| `agents-room hook` | short-lived, per harness event | wake signals, join/leave |
| `agents-room` | on demand | CLI: `who`, `send`, `lease`, `watch` |

Why this is better here:

- **No port, no PID file, no orphan process, no "is the daemon up?"**
- **Crash-proof.** A killed agent cannot take the shared state with it.
- **Cross-repository for free.** One store, rooms are logical names.
- **Aligned with the MCP direction.** The 2026-07-28 revision removed sessions
  and server-initiated requests; a stateless server behind an explicit handle
  is the sanctioned shape.
- **Cheap polling.** Every hook event is one indexed `SELECT` on a local file.

What we give up: an event-driven long-poll. In practice the harness hook
boundary is where a wake signal is useful anyway, so nothing is lost.

Concurrency is handled by SQLite: WAL, `busy_timeout`, short transactions,
`BEGIN IMMEDIATE` for lease acquisition.

## Data model

```
room        (id, name, created_at)
session     (id, room_id, harness, cwd, pid, label, tags, joined_at, last_seen)
message     (id, room_id, sender, selector, subject, body, state,
             reply_to, created_at, expires_at)
lease       (room_id, path_pattern, owner, mode, acquired_at, expires_at)
file_read   (room_id, path, session, content_hash, read_at)
event       (id, room_id, ts, kind, payload)   -- append-only
```

Leases are written as **append-only events** and the `lease` table is a
materialized view rebuilt on write. This keeps an audit trail of who claimed
what and when, and makes recovery from a crashed holder trivial.

## Delivery states

Honest states. We never claim delivery we cannot prove.

| State | Meaning |
|---|---|
| `stored` | Written to the store |
| `offered` | A wake signal was emitted toward the recipient harness |
| `accepted` | The harness accepted the signal (socket write returned OK) |
| `read` | The recipient agent fetched the body with `msg_recv` |
| `acked` | The recipient explicitly confirmed with `msg_ack` |
| `expired` | TTL passed before it was read |
| `undeliverable` | No wake lever exists for this harness; waits for pull |

The wake signal **never carries the message body**. It carries one line:

```
[agents-room] 2 unread in room "aerobasis"; call msg_recv()
```

The body arrives as a tool result, which keeps context small and makes
`read` mean something.

## Address selectors

| Selector | Resolves to |
|---|---|
| `*` | every session in the room except the sender |
| `<session-id>` | one session |
| `file:<glob>` | sessions holding a lease matching the glob |
| `tag:<tag>` | sessions whose join tags include the tag |

`file:` and `tag:` are the differentiators: nobody else answers "who is
touching `src/api/users.go`", and nobody answers it across repositories at
all.

## MCP tools

```
room_join(room, label?, tags?) -> session_id
room_leave()
room_who(room?) -> sessions
msg_send(to, subject, body, reply_to?, ttl_s?) -> message_id
msg_recv(limit?) -> messages
msg_ack(message_id)
lease_claim(path, ttl_s, mode?) -> {ok, holder?}
lease_release(path)
lease_check(path) -> {holder, expires_at} | null
lease_list() -> leases
note_read(path) -> content_hash
drift_check(path) -> {severity, changed_by, since}
history(since?, limit?) -> events
```

`drift_check` returns `SAFE`, `MINOR` (file changed, no lease conflict) or
`CRITICAL` (you read it, someone else holds a lease and wrote it).

## Wake levers per harness

| Harness | Lever | Latency |
|---|---|---|
| Claude Code | `PostToolBatch` hook → post nudge to `CLAUDE_CODE_MESSAGING_SOCKET` | per tool batch |
| Claude Code | `Stop` hook → `decision:"block"` with one-line reason | end of turn |
| Claude Code | `Notification` / `idle_prompt` hook → post to own socket | ≤ 60 s idle |
| Claude Code | `asyncRewake` hook → exit 2 | immediate |
| Codex CLI | `PostToolUse` / `Stop` hooks | per tool call / turn end |
| Hermes | pull only, until an adapter exists | next tool call |
| anything MCP | pull only (`msg_recv`) | model decides |

The hook binary is one program with a `--harness` flag; each harness gets a
thin config snippet, not a separate codebase.

## Components

```
cmd/agents-room          the single binary: CLI, `mcp` and `hook` subcommands
internal/store           SQLite schema, migrations, queries
internal/room            rooms, sessions, selectors
internal/lease           claim/release/expiry, append-only events
internal/drift           read tracking, hash comparison
internal/wake            per-harness wake levers
internal/mcp             tool definitions and handlers
docs/                    protocol, architecture, harness setup
```

## Milestones

| # | Scope | Done when |
|---|---|---|
| M0 | Repo scaffolding: GPLv3, README, docs skeleton, CI, `make` | `make build && make test` green, repo public |
| M1 | Store + rooms + sessions + messages + leases + CLI | two terminals exchange a message and contend for a lease |
| M2 | MCP server | Claude Code and Codex both see the tools |
| M3 | Claude Code wake adapter | idle session wakes on a nudge, end-to-end |
| M4 | Drift + `file:`/`tag:` selectors | stale read is detected and reported |
| M5 | `watch` view | human sees the room live |
| M6 | Codex/Hermes adapters + docs | setup is copy-paste per harness |
| M7 | Release: static binaries, signed checksums | install from a release page |

## Testing

- Unit: store transactions, lease expiry, selector resolution, drift severity.
- Integration: two in-process sessions against a temp DB; concurrent lease
  claim under `-race`; message TTL expiry.
- Harness: a fake socket server that records what the hook posts.
- No test touches `~/.agents-room`; every test uses `t.TempDir()`.

## Decisions (2026-11-06)

1. **Language: Go.** Single static binary on Linux/Windows/macOS, no runtime,
   `modernc.org/sqlite` needs no cgo. Toolchain present: go1.24.5, gcc 15.2.
2. **Rooms: repository room + shared rooms.** A session joins
   `repo:<basename>` automatically and may join any number of named shared
   rooms (`aerobasis`) to coordinate API contracts across related repositories.
   One session, several rooms, one store. Implemented with a many-to-many
   `session_rooms` table in schema version 3.
3. **Autonomy is a negotiated, three-level setting**, not a boolean.

## Autonomy levels

Each session runs at one of three autonomy levels. The level decides whether a
wake signal may interrupt the agent and whether the end of a turn may be held.

| Level | Name | Stop hook | What it is for |
|---|---|---|---|
| 0 | `notify` | one-line signal, never blocks | operator is present, wants no interruptions |
| 1 | `hold` | holds the end of a turn until unread is fetched | active contention, a long important task in progress |
| 2 | `autonomous` | may hold, and may start a new turn from idle | operator is away, agents are left to themselves |

Level 1 and 2 differ only in whether an idle session may be woken; both hold
the turn end. Level 0 never holds.

### Negotiation

The level is not a silent config. It is **negotiated with the operator**:

- `room_join(..., autonomy=?)` states the level the agent proposes.
- The effective level is `min(agent_proposed, room_cap, operator_cap)`.
- The operator sets a cap per room and per session; a session can lower its
  own level at any time and cannot raise it above the cap.
- `room_who()` shows every session's effective level, so an operator always
  knows who is allowed to interrupt whom.
- A harness that has no wake lever reports `undeliverable`; its level has no
  effect on delivery and the room says so explicitly rather than pretending.

### Why three levels

The operator's own words: with heavy contention or a long important task,
level 1 is right; when away from the keyboard, level 2 lets the agents keep
working and wake each other; when sitting nearby, level 0 is enough. A boolean
would force one of those cases to be wrong.

## Open questions

None blocking. Revisit after M3: whether `hold` should have a per-turn cap
lower than the harness default (Claude Code caps at 8 consecutive
continuations).

## Status at 2026-11-06

| Milestone | State |
|---|---|
| M0 scaffolding: GPLv3, README, docs, CI, Makefile | done |
| M1 store, rooms, sessions, messages, leases, CLI | done, tested |
| M1.5 schema migrations and idempotent join | done, tested |
| M2 MCP server | done, tools registered |
| M3 Claude Code wake adapter | done, verified against a live inbox socket |
| M4 drift and `file:`/`tag:` selectors | done, tested |
| M5 `watch` view | done, polling |
| M6 Codex and Hermes adapters | both have dedicated adapters built against their source contracts; Hermes verified live, Codex verified by contract only |
| M6.5 renew, prune, autonomy cap | done, tested |
| M6.6 session identity | `liveness` from the harness key, `pid_started_at` against pid reuse, duplicate chats collapsed by key with their deliveries carried over; schema version 5 |
| M6.7 room history, path warnings, watch messages, unhooked-session check | `room_history` and `agents-room history`, `message_rooms` so a shared room's history is complete, warnings for a lease or `file:` selector that names another repository, messages in `watch`, and a doctor check for a live session whose hook is not running; schema version 6 |
| M7 release packaging | cross-builds for six targets, a tag workflow publishing checksums, and `make install` |

Open items are listed in [`CONTEXT.md`](CONTEXT.md).
