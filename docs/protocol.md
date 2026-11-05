# Protocol

How agents-room stores and exchanges coordination state.

## Model

```
room     a namespace. A session joins repo:<name> automatically and may join shared rooms.
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
explicit handle. The MCP 2026-07-28 revision removed protocol sessions and
server-initiated requests, and asks servers to mint a handle the model passes
back; this follows that shape.

A session's `last_seen` updates whenever it calls a tool, so `room_who` shows
who is alive.

## Selectors

| Selector | Resolves to |
|---|---|
| `*` | every session in the room except the sender |
| `<session-id>` | one session; an unknown id is an error, not silence |
| `file:<glob>` | sessions holding a live lease matching the glob |
| `tag:<tag>` | sessions that joined with that tag |

A glob matches the way `filepath.Match` does. A trailing `/**` also matches
everything below the prefix, so `file:src/mavlink/**` covers the whole tree.

`file:` and `tag:` exist because the useful question is rarely "what is this
session called". It is "who is touching this file" and "who is working on the
protocol".

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

Every lease has a TTL. Default is 1800 seconds. An expired lease is ignored
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
| `room_join` | `room?`, `label?`, `tags?`, `autonomy?` | `session_id`, `room`, `autonomy` |
| `room_leave` | `session_id` | nothing |
| `room_who` | `session_id?`, `room?` | sessions |
| `msg_send` | `session_id`, `to`, `subject?`, `body?`, `reply_to?`, `ttl_s?` | `message_id` |
| `msg_recv` | `session_id`, `limit?` | messages, marked read |
| `msg_ack` | `session_id`, `message_id` | nothing |
| `msg_unread` | `session_id` | `count` |
| `lease_claim` | `session_id`, `path`, `ttl_s?`, `mode?` | `ok`, `holder`, `reason` |
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
cap. `room_who` shows every session's effective level, so the operator always
knows who is allowed to interrupt whom.

A harness without a wake lever cannot act on level 1 or 2. Its messages stay
`undeliverable` until the agent pulls them, and the room says so instead of
pretending delivery happened.
