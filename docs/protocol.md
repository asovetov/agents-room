# Protocol

How agents-room stores and exchanges coordination state.

## Model

```
room     a namespace. A session joins repo:<name> automatically and may join
         any number of shared rooms, which is how agents in different
         repositories reach each other.
session  one agent process: harness, cwd, pid, label, tags, autonomy, last_seen
message  one send: sender, selector, subject, body, created_at, expires_at
delivery one row per resolved recipient, holding the delivery state
lease    a claim on a path: owner, mode, acquired_at, expires_at
read     the content hash a session last saw for a path
event    append-only audit row: session.join, session.leave, message.send, lease.claim, lease.release
```

The store is one SQLite database in WAL mode at `~/.agents-room/room.db`.
Override with `AGENTS_ROOM_DB`. There is no daemon.

## Sessions

`room_join` returns a session id. Every other tool takes it back as an
explicit handle.

Passing `harness_key` makes a join idempotent, and an MCP server spawned by a
harness fills it from the harness session id it inherited
(`CLAUDE_CODE_SESSION_ID` and its equivalents) when the model omits it. Claude Code fires
`SessionStart` on startup, resume, clear, compact and fork, and Hermes fires
`on_session_start` on each new chat; without the key, one chat would appear
as five sessions. The key is the harness's own session id. The MCP 2026-07-28 revision removed protocol sessions and
server-initiated requests, and asks servers to mint a handle the model passes
back; this follows that shape.

A session's `last_seen` updates whenever it calls a tool, so `room_who` shows
who is alive. The MCP server does this in one middleware over `tools/call`,
so a tool added later inherits it without its own `Touch` call. `prune` reads
the same membership table as `room_who`, so a session reachable only through
a shared room can still be swept from that room.

Because `last_seen` only moves on a tool call or a nudge, a quiet session can
be very much alive. `prune` therefore removes a session only when it has also
been quiet longer than the threshold **and** its pid is gone; it releases no
leases for a process that still exists.

The pid has to be the harness process, not the process that joined: `join`
and `mcp` are short-lived children of the chat, so their own pid is dead
before anyone looks. Nor is the immediate parent enough, because a hook runs
under a shell that exits at once. A session records `AGENTS_ROOM_PID` when it
is set; otherwise the process chain is walked until a known harness name
(`claude`, `codex`, `hermes`) appears. When none is found, the session
records no pid and prune keeps it, which is the safe direction.

A shared room is spelled `shared:<name>`. An extra room named without a
prefix gets one at join time, because `--also storefront` and
`AGENTS_ROOM_SHARED=storefront` are the same room to a human and were two
rows to the store, each holding half the membership.

Membership is many-to-many: `session_rooms` holds one row per (session, room).
A chat in `gcs` that also joined `shared:storefront` is the same session in
both rooms, with one session id. Rejoining with the same `harness_key` from a
different room updates that session instead of creating a second one.

The effective autonomy is the strictest cap of every room the session is in,
so a quiet shared room cannot be bypassed by a loud repository room.

Shared rooms can be discovered rather than named: `AGENTS_ROOM_SHARED` and
`<repo>/.agents-room/shared-rooms` are read by the hook at `SessionStart`, so
every agent in a repository joins the same shared rooms without a flag.

A session sends to the union of its rooms. A direct session id must share at
least one room with the sender; anything else is an error, not a silent
no-op.

## Selectors

| Selector | Resolves to |
|---|---|
| `*` | every session in the room except the sender |
| `<session-id>` | one session; an unknown id is an error, not silence |
| `file:<glob>` | sessions holding a live lease matching the glob |
| `tag:<tag>` | sessions that joined with that tag |

Paths are normalised to absolute form before they are stored, so
`src/wire.go` and `/abs/src/wire.go` from the same directory contend for one
lease. A file selector is matched against the leased path in three forms:
the full path, the path relative to the **holder's** working directory, and
the bare base name.

The holder's directory is the anchor, not the room's: a shared room can hold
sessions from several repositories, and `src/wire.go` means a different file
in each of them.

Lease contention is between sessions, not between room rows. A claim checks
every session that shares at least one room with the claimant, so two agents
that meet only in a shared room still cannot hold one file at the same time. A trailing `/**` also matches everything below the prefix,
so `file:src/api/**` covers the whole tree.

Matching uses `path.Match`, not `filepath.Match`: on Windows the latter
treats the backslash as the separator, so `src/*.go` would match
`src/sub/wire.go` there.

`file:` and `tag:` exist because the useful question is rarely "what is this
session called". It is "who is touching this file" and "who is working on the
protocol".

A selector that matches nobody is not an error: a broadcast in a room of one,
or a `file:` selector on a path only the sender holds, is a legitimate send
with zero recipients. The count is reported back instead of being hidden, so
a sender does not mistake a stored message for a delivered one. The CLI
prints a warning on stderr; `msg_send` returns `recipients`, and `msg_recv`
reports the same count for the message it just delivered.

## Delivery states

Each recipient gets its own row. State only moves forward.

| State | Meaning |
|---|---|
| `stored` | written, no wake signal attempted |
| `offered` | a wake signal was emitted but its acceptance is unknown |
| `accepted` | the harness took the signal (the socket write succeeded) |
| `read` | the recipient fetched the body with `msg_recv` |
| `acked` | the recipient confirmed with `msg_ack` |
| `expired` | TTL passed before the body was read |
| `undeliverable` | this harness has no wake lever; the message waits for a pull |

A wake signal never carries the body. It carries one line:

```
[agents-room] 2 unread in room "repo:gcs"; call msg_recv
```

That keeps the recipient's context small, and it makes `read` mean that the
recipient actually saw the content rather than that a notification arrived.

## Leases

`lease_claim` is the only operation that must be atomic across processes.
It runs inside `BEGIN IMMEDIATE`, sweeps expired leases, checks the holders,
and inserts. Two processes claiming the same path cannot both succeed.

| Mode | Rule |
|---|---|
| `exclusive` | one holder; blocks every other claim |
| `shared` | several holders; blocks exclusive claims, allows other shared ones |

Every lease has a TTL. Default is 1800 seconds. `lease_renew` extends one a
session already holds and never takes a path it does not hold, so a stale
agent cannot re-acquire a file someone else has since claimed. An expired lease is ignored
by every read and removed by the next claim, so a crashed session cannot hold
a file forever. `room_leave` releases everything the session held.

## Drift

`note_read` stores the SHA-256 of a file at the moment a session read it.
`drift_check` compares the file on disk with that hash.

| Severity | Condition |
|---|---|
| `SAFE` | unchanged, or the session never recorded a baseline |
| `MINOR` | changed, and no other session holds a lease on it |
| `CRITICAL` | changed while another session held a lease |

`CRITICAL` means the file moved under you while someone else was responsible
for it. Re-read before editing.

## Tool reference

| Tool | Arguments | Returns |
|---|---|---|
| `room_join` | `room?`, `extra_rooms?`, `label?`, `tags?`, `autonomy?`, `harness?`, `harness_key?` | `session_id`, `rooms`, `autonomy` |
| `room_leave` | `session_id` | nothing, leaves every room |
| `room_leave_room` | `session_id`, `room` | nothing, stays in the rest |
| `room_who` | `session_id?`, `room?` | sessions |
| `room_set_autonomy_cap` | `session_id?`, `room?`, `autonomy` | `room`, `autonomy` |
| `msg_send` | `session_id`, `to`, `subject?`, `body?`, `reply_to?`, `ttl_s?` | `message_id`, `recipients` |
| `msg_recv` | `session_id`, `limit?` | messages, marked read |
| `msg_ack` | `session_id`, `message_id` | nothing |
| `msg_unread` | `session_id` | `count` |
| `lease_claim` | `session_id`, `path`, `ttl_s?`, `mode?` | `ok`, `holder`, `reason` |
| `lease_renew` | `session_id`, `path`, `ttl_s?` | the renewed lease |
| `lease_release` | `session_id`, `path` | nothing |
| `lease_check` | `session_id`, `path` | `free`, `lease` |
| `lease_list` | `session_id` | leases |
| `note_read` | `session_id`, `path` | `hash` |
| `drift_check` | `session_id`, `path` | `path`, `severity`, `changed`, `holder`, `since`, `message` |

## Autonomy

Autonomy decides whether a wake signal may interrupt the agent.

| Level | Name | Behaviour |
|---|---|---|
| 0 | `notify` | signals, never holds the end of a turn |
| 1 | `hold` | holds the end of a turn until unread is fetched |
| 2 | `wake` | may also start a turn in an idle session |

The effective level is the minimum of what the agent asks for and the room
cap. The cap is per room and defaults to `wake`; an operator lowers it with
`room_set_autonomy_cap` or `agents-room cap --set N`. Sessions already
running keep their level until they rejoin, so raising a cap never silently
grants interrupt rights to a session that did not ask for them. `room_who` shows every session's effective level, so the operator always
knows who is allowed to interrupt whom.

A harness without a wake lever cannot act on level 1 or 2. Its messages stay
`undeliverable` until the agent pulls them, and the room says so instead of
pretending delivery happened.
