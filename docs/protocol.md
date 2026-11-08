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

**An unknown session id is an error, not an empty answer.** A read for an id
that names nothing returns no rows, which is exactly what an empty inbox
returns; a caller that passed the wrong id cannot tell the two apart. One
agent called `msg_recv` with its harness session id for weeks, was told
"no unread messages" every time, and concluded nothing had been sent to it
while nineteen messages piled up in the room. The error names the id the
caller probably meant: an exact harness key is resolved to its session, a
truncated id is matched against session ids and labels, and an id of the right
shape that names nothing is reported as possibly left or from another store.
The same check guards `msg_unread`, `msg_sent`, `msg_ack` and every lease and
drift call, and the room-resolving tools refuse instead of falling back to the
current directory's room — a mistyped id there used to write an autonomy cap
into a room the caller is not in and report success.

Passing `harness_key` makes a join idempotent, and an MCP server spawned by a
harness fills it from the harness session id it inherited
(`CLAUDE_CODE_SESSION_ID` and its equivalents) when the model omits it. Claude Code fires
`SessionStart` on startup, resume, clear, compact and fork, and Hermes fires
`on_session_start` on each new chat; without the key, one chat would appear
as five sessions. The key is the harness's own session id.

A key that is not the caller's own `CLAUDE_CODE_SESSION_ID` is a warning, not
a refusal. Passing the wrong one adopts another chat's session silently: the
join succeeds and the caller starts answering messages addressed to someone
else. The warning names both ids and goes to stderr in the CLI, so the session
id on stdout stays parseable, and into `msg_send`-style text in MCP.

The MCP 2026-07-28 revision removed protocol sessions and
server-initiated requests, and asks servers to mint a handle the model passes
back; this follows that shape.

A session's `last_seen` updates whenever it calls a tool. The MCP server does
this in one middleware over `tools/call`, so a tool added later inherits it
without its own `Touch` call. `prune` reads the same membership table as
`room_who`, so a session reachable only through a shared room can still be
swept from that room.

**`last_seen` is activity, not liveness.** A chat thinking through a long
edit and a chat that exited an hour ago are indistinguishable by it, so
`room_who` reports a separate `liveness` of `alive`, `gone` or `unknown`:

- `alive` and `gone` are decided from the **harness key**. The scan of running
  processes is asked which keys are in any process's environment; a chat
  keeps its key across MCP restarts, which the pid does not.
- `unknown` means the row records no pid at all, which is not the same as
  gone. The join could not find a harness process, and guessing would delete
  a live chat's leases.

`prune` removes a session only when it has been quiet longer than the
threshold **and** it is `gone`; it releases no leases for a chat that still
exists. It decides `gone` with the same function `room_who` uses, from the
same scan. The two must not disagree: a chat whose MCP server was restarted
has a working harness key and a stale pid, so a pid-based prune would delete
a session `room_who` still shows as alive.

A pid alone is not an identity. When a chat exits its number is free, and the
next process the OS hands it to is a stranger: an "is this pid alive" check
then answers about the stranger and calls a dead session present. Each row
therefore records `pid_started_at` with the pid, and the pair names one
process. A row written before the column existed has `0` there and falls back
to the pid, which is the older, weaker check.

The pid has to be the harness process, not the process that joined: `join`
and `mcp` are short-lived children of the chat, so their own pid is dead
before anyone looks. Nor is the immediate parent enough, because a hook runs
under a shell that exits at once. A session records `AGENTS_ROOM_PID` when it
is set; otherwise the process chain is walked until a known harness name
(`claude`, `codex`, `hermes`) appears. When none is found, the session
records no pid and prune keeps it, which is the safe direction.

`Touch` refreshes the pid along with `last_seen`, because a tool call proves
which process the chat is running as now. Without that the pid goes stale the
first time the MCP server is restarted under a live chat. The refresh applies
only to the row this process owns, matched by harness key, so an operator
touching another session cannot attach its own pid to it.

Only Linux reads `/proc`, so the key scan, the start time and the pid are all
available there and liveness is exact. On Windows and macOS those helpers
return nothing, liveness falls back to "is the pid running", and a session
records no pid at all when no harness process can be recognised — which is
the safe direction: an unknown session is never pruned. On those platforms
`prune` and `room_who` still agree, because both take the same fallback.

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

Knowing how many were addressed is not the same as knowing whether they read
it. `msg_sent` (and `agents-room sent`) lists what a session sent with a
per-state tally and a `pending` count, so a sender can tell a slow reader from
a message nobody ever picked up.

A recipient that leaves with the message unread is reported separately as
`left`, not as pending. Its delivery is deleted with the session, so counting
only the rows on disk would say "still waiting" about a session that no
longer exists. The recipient count recorded in the `message.send` event is
what makes the difference visible: `sent == recipients + left`.

### A message belongs to every room it reaches

Deliveries answer "what is waiting for me". They cannot answer "what has been
decided here", because they are created at send time: a session that joins
afterwards has none, and `msg_recv` correctly returns nothing.

`room_history` answers the second question. It reads `message_rooms`, which
holds one row per (message, room) for every room the sender was in when it
sent. That is what makes a shared room's history complete: an agent in
`repo:bot` writing to an agent it meets in `shared:storefront` leaves the
message findable in both, and the shared room does not read as empty while
work is plainly going on.

History is read-only and never marks a delivery read. A session that catches
up this way has not acknowledged anything addressed to it, and saying
otherwise would make the delivery states lie.

`room_join` returns one line when a room saw traffic in the last day, naming
the count and the latest subject, so the joining agent knows to call
`room_history` instead of trusting `msg_recv`'s silence.

### An empty inbox says why it is empty

`msg_recv` returning nothing is correct and, on its own, uninformative: it is
the same answer for a session that has caught up and for one that is in the
wrong room. So the empty answer carries an `inbox` block — the rooms the
session is in, how many deliveries it has ever had, how many it has read, and
when the last one was created.

That turns the two cases into different sentences: nothing has ever been sent
to this session (check that the sender and you share a room), or N messages
were delivered and all of them read (there is nothing more to do). An agent
that suspected it had written to the wrong room previously had to read the
database by hand to find out; `agents-room recv` prints the same summary.

### A signal is retried, but never twice inside one turn

There is no `offered -> stored` transition, so a delivery that was offered
once used to be invisible to the wake check for good. At wake level the
socket confirms the offer as `accepted` and the turn arrives; at hold level
the delivery stayed in `offered` and the chat was never told again. A live
store had a session with eleven offered and zero read deliveries.

`wake.Check` now takes a retry window, and the hook picks it by event:

| Event | Window | Why |
|---|---|---|
| `Stop` | 30 minutes | Stop can hold a turn; re-signalling inside one starts another turn whose Stop blocks again |
| tool events, `UserPromptSubmit` | 0 | they add context to a result and cannot extend a turn |

Only `offered` is retried. `accepted` means the harness took the signal, so
the turn is already coming and repeating it would start a second one.

### A path is relative to your own checkout

`file:` patterns and lease paths are resolved against the caller's working
directory. That is the only sensible rule, and it is also the trap: an agent
meaning another repository's `src/users.go` writes a relative path, leases its
own `src/users.go`, and both sides believe they agreed.

Both operations therefore warn when the result looks wrong rather than
refusing, because cross-repository leases are exactly what the tool is for:

- `lease_claim` warns when the claimed path is outside the caller's own
  directory.
- `msg_send` warns when a `file:` selector matched nobody, names the absolute
  path the pattern meant from the sender's directory, and says to use the
  absolute path to reach another repository.

## Delivery states

Each recipient gets its own row. State only moves forward.

A delivery belongs to a session, so it goes when the session goes: `leave`
and `prune` delete the session's rows, and schema version 4 dropped the ones
left behind by earlier versions. A row whose session is gone can never be
read and would still be counted as unread.

| State | Meaning |
|---|---|
| `stored` | written, no wake signal attempted |
| `offered` | a wake signal was emitted but its acceptance is unknown |
| `accepted` | the harness took the signal (the socket write succeeded) |
| `read` | the recipient fetched the body with `msg_recv` |
| `acked` | the recipient confirmed with `msg_ack` |
| `expired` | TTL passed before the body was read |
| `undeliverable` | this harness has no wake lever; the message waits for a pull |

A wake signal is emitted once per message. `offered` and `accepted` still
count as unread, because the agent has not fetched the body yet, but they no
longer trigger another signal: the hook only signals a `stored` row. Signalling
again at wake level starts a second turn for a message the first turn was
already about, and a turn whose `Stop` signals again never ends. Once signalled,
a message waits for `msg_recv`.

A wake signal never carries the body. It carries one line:

```
[agents-room] 2 unread in rooms "shared:storefront", "repo:storefront-web"; call msg_recv
```

The rooms named are the ones the unread messages **came through**, not the
session's first room. A session joins its repository room first and its shared
rooms after, so naming the first one is almost always wrong for the
cross-repository case the tool exists for: a link in `repo:storefront-web`
meeting the firmware in `shared:storefront` was told its message was in
`repo:storefront-web` and went looking where nothing had changed. The list is
intersected with the reader's own membership — naming a room the reader is not
in would be a different wrong answer — and past two rooms the line counts the
rest rather than growing.

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

A lease is meant to cover the edit it guards, and an edit can outlast the
TTL. The hook renews every lease its session holds on each tool event, so a
session that is doing anything keeps them without naming each path, and one
that stops working loses them after the same half hour as before. A lapsed
lease is never taken back by renewal: someone else may hold the path by now.
`doctor` warns when a live lease has less than a quarter of its life left,
which usually means the harness is not calling the hook.

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
