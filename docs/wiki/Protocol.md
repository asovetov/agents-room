**English** · **Русский:** [Протокол](Protocol-ru)

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
event    append-only audit row
```

The store is one SQLite database in WAL mode at `~/.agents-room/room.db`.
Override with `AGENTS_ROOM_DB`. There is no daemon.

## Sessions

`room_join` returns a session id. Every other tool takes it back as an
explicit handle.

**An unknown session id is an error, not an empty answer.** A read for an id
that names nothing returns no rows, which is exactly what an empty inbox
returns; a caller that passed the wrong id cannot tell the two apart. One
agent called `msg_recv` with its harness session id for weeks, was told
"no unread messages" every time, and concluded nothing had been sent to it
while nineteen messages piled up in the room.

The error names the id the caller probably meant:

- an exact **harness key** resolves to its session;
- a truncated id is matched against session ids and labels;
- an id of the right shape that names nothing is reported as possibly left,
  or from another store.

### The two ids

| Id | Shape | Where it goes |
|---|---|---|
| harness key | UUID, e.g. `2b50133e-...` | `room_join` as `harness_key`; the harness puts it in the environment |
| session id | 16 lowercase hex | everywhere else, as `session_id` |

They look alike to a model holding both, and passing the wrong one used to be
invisible.

### `last_seen` is activity, not liveness

A chat thinking through a long edit and a chat that exited an hour ago are
indistinguishable by it, so `room_who` reports a separate `liveness` of
`alive`, `gone` or `unknown`:

- `alive` and `gone` are decided from the **harness key** — the scan of running
  processes is asked which keys are in any process's environment. A chat keeps
  its key across MCP restarts, which the pid does not.
- `unknown` means the row records no pid at all, which is not the same as gone.

`prune` removes a session only when it has been quiet longer than the
threshold **and** it is gone. It decides gone with the same function
`room_who` uses, from the same scan.

### A rejoin raises autonomy, never lowers it

Only `wake` can start a turn in an idle chat. Below it the signal rides a turn
the agent is already taking, so an idle session at `notify` or `hold` is
unreachable and looks exactly like a broken hook.

The level is kept unless a rejoin explicitly raises it:

- **not mentioned** — the stored level is kept;
- **higher** — applied, clamped by the room cap;
- **lower** — refused, with a warning naming the level that stayed.

Lowering is the operator's decision and has one route: the room cap.

## Selectors

| Selector | Resolves to |
|---|---|
| `*` | every session in the room except the sender |
| `<session-id>` | one session; an unknown id is an error, not silence |
| `file:<glob>` | sessions holding a live lease matching the glob |
| `tag:<tag>` | sessions that joined with that tag |

Paths are normalised to absolute form before they are stored, so
`src/wire.go` and `/abs/src/wire.go` from the same directory contend for one
lease. A file selector is matched against the leased path in three forms: the
full path, the path relative to the **holder's** working directory, and the
bare base name.

The holder's directory is the anchor, not the room's: a shared room can hold
sessions from several repositories, and `src/wire.go` means a different file
in each of them.

A selector that matches nobody is not an error: a broadcast in a room of one
is a legitimate send with zero recipients. The count is reported back, so a
sender does not mistake a stored message for a delivered one.

## A message belongs to every room it reaches

Deliveries answer "what is waiting for me". They cannot answer "what has been
decided here", because they are created at send time: a session that joins
afterwards has none.

`room_history` answers the second question. It reads `message_rooms`, which
holds one row per (message, room) for every room the sender was in when it
sent. That is what makes a shared room's history complete.

History is read-only and never marks a delivery read.

## An empty inbox says why it is empty

`msg_recv` returning nothing is the same answer for a session that has caught
up and for one in the wrong room. So the empty answer carries an `inbox`
block: the rooms the session is in, how many deliveries it has ever had, how
many it read, and when the last one was created.

## Delivery states

Each recipient gets its own row. State only moves forward.

| State | Meaning |
|---|---|
| `stored` | written, no wake signal attempted |
| `offered` | a wake signal was emitted but its acceptance is unknown |
| `accepted` | the harness took the signal |
| `read` | the recipient fetched the body |
| `acked` | the recipient confirmed |
| `expired` | TTL passed before the body was read |
| `undeliverable` | this harness has no wake lever |

A wake signal is emitted once per message, then retried on tool events once
it is older than the retry window. Only `offered` is retried: `accepted` means
the turn is already coming, and repeating it would start a second one.

## Leases

`lease_claim` is the only operation that must be atomic across processes. It
runs inside `BEGIN IMMEDIATE`, sweeps expired leases, checks the holders, and
inserts.

| Mode | Rule |
|---|---|
| `exclusive` | one holder; blocks every other claim |
| `shared` | several holders; blocks exclusive claims, allows other shared ones |

## Drift

`note_read` stores the SHA-256 of a file at the moment a session read it.
`drift_check` compares the file on disk with that hash.

| Severity | Condition |
|---|---|
| `SAFE` | unchanged, or the session never recorded a baseline |
| `MINOR` | changed, and no other session holds a lease on it |
| `CRITICAL` | changed while another session held a lease |

## Tool reference

| Tool | Arguments | Returns |
|---|---|---|
| `room_join` | `room?`, `extra_rooms?`, `label?`, `tags?`, `autonomy?`, `harness?`, `harness_key?` | `session_id`, `rooms`, `autonomy`, `warning?` |
| `room_leave` | `session_id` | nothing, leaves every room |
| `room_leave_room` | `session_id`, `room` | nothing, stays in the rest |
| `room_who` | `session_id?`, `room?` | sessions, each with `liveness` |
| `room_set_autonomy_cap` | `session_id?`, `room?`, `autonomy` | `room`, `autonomy` |
| `msg_send` | `session_id`, `to`, `subject?`, `body?`, `reply_to?`, `ttl_s?` | `message_id`, `recipients`, `warning?` |
| `msg_recv` | `session_id`, `limit?` | messages, marked read; `inbox` when empty |
| `msg_ack` | `session_id`, `message_id` | nothing |
| `msg_unread` | `session_id` | `count` |
| `msg_sent` | `session_id`, `limit?` | `messages`: each with `states`, `pending`, `left`, `sent` |
| `room_history` | `session_id?`, `room?`, `limit?` | `room`, `messages` (read-only) |
| `lease_claim` | `session_id`, `path`, `ttl_s?`, `mode?` | `ok`, `holder`, `reason` |
| `lease_renew` | `session_id`, `path`, `ttl_s?` | the renewed lease |
| `lease_release` | `session_id`, `path` | nothing |
| `lease_check` | `session_id`, `path` | `free`, `lease` |
| `lease_list` | `session_id` | leases |
| `note_read` | `session_id`, `path` | `hash` |
| `drift_check` | `session_id`, `path` | `path`, `severity`, `changed`, `holder`, `since`, `message` |
