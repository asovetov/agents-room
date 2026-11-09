# Context for a new session

Read this first. It records what is decided, what is open, and where to start.

## What this project is

`agents-room` is a coordination layer for AI coding agents that work in
**different repositories** and have to agree with each other — an API in one
repository, the clients that call it in others, each with its own sessions
and often its own harness. It provides repository rooms plus shared rooms
that cross them, session presence, file leases, drift detection and
addressing by file or tag, over MCP.

The single-repository case — several sessions on one codebase — works the
same way and is the simpler one, but it is not the point of the project. The
reason it exists is that agents in separate repositories cannot otherwise
see each other at all.

Author of the idea: Anton Sovetov. License: GPLv3, permanently, never sold.

## Read in this order

1. [`../README.md`](../README.md): the problem and the shape of the answer.
2. [`plan.md`](plan.md): scope, architecture, milestones, open questions.
3. [`protocol.md`](protocol.md): data model, selectors, delivery states, tools.
4. [`harness-setup.md`](harness-setup.md): per-harness installation.
5. [`history/`](history/): the original Russian drafts, for provenance only.

## Decided

| Question | Decision |
|---|---|
| Name | `agents-room` |
| License | GPLv3, permanent, never sold |
| Language | Go, single static binary, no cgo |
| Platforms | Linux, macOS, Windows on amd64 and arm64 |
| Transport | none: SQLite in WAL mode, no daemon |
| Interface | MCP server, stateless, explicit session handle |
| Rooms | `repo:<name>` automatically, plus named shared rooms |
| Delivery | one-line nudge, body fetched by the agent |
| Autonomy | three levels, negotiated with the operator |
| Dependencies | none copied from other projects |

## Open

1. A live Codex check. The adapter follows the Codex source contract
   (Stop needs the JSON envelope, tool events need it too) but Codex is not
   installed on the author's machine, so it has not been run end to end.
2. Whether `hold` should have a per-turn cap lower than the harness default.
3. Automatic pruning. `agents-room prune` exists, but no timer runs it.
4. Sending into a room where you are the only member, or to a `file:`
   selector on a path only you hold, still stores a message with zero
   recipients. The count is reported back by `msg_send` and `msg_recv`
   rather than the send being rejected: a broadcast in a room of one is
   legitimate. Revisit if agents keep missing it.
5. Rooms created before join normalised the prefix stay split, because
   merging them would move sessions between rooms without their consent.
   `doctor` names each split and the two commands that repair it, and
   `doctor --fix` collapses the duplicate-chat case but deliberately leaves a
   split room alone: moving a session between rooms is the session's call.
   A split was also how one chat came to be reported twice, so the two
   findings were often the same defect seen from two sides; collapsing the
   duplicate can make the split finding go away with it.
6. A wake signal is retried on tool events but not on `Stop`. The retry is
   what makes a message offered once still announce itself; the `Stop`
   exemption is what keeps a blocked turn from starting another one. Any
   change to `wake.Check` or `retryWindow` has to keep both halves.
7. A message belongs to every room the sender was in, through `message_rooms`
   (schema version 6). That is what makes a shared room's history complete.
   It also means one message can appear in several rooms' histories, which is
   correct but worth remembering when counting traffic.
8. A rejoin raises a session's autonomy, never lowers it, and `doctor`
   reports every live session below `wake`. Only `wake` can start a turn in
   an idle chat, so a chat below it is unreachable and looks exactly like a
   broken hook. It used to happen silently: `room_join` over MCP sent
   `autonomy` as a plain int with `omitempty`, a call that omitted the field
   arrived as `0`, and the rejoin wrote that over the level the `SessionStart`
   hook had set. Two live sessions sat at `notify` this way. The field is now
   `*int` on the wire and `JoinOptions.AutonomySet` in the hub, so "not
   mentioned" and "asked for notify" are different things; lowering is the
   cap's job alone, and a rejoin that asks for it is told so.
9. A wake signal is posted to the **recipient's** socket, recorded in
   `sessions.wake_socket` at join time (schema version 7). `CLAUDE_CODE_MESSAGING_SOCKET`
   names the socket of the chat whose process is running, and the hook that
   emits the signal runs under the sender — so reading it there posted the
   signal to the sender's own inbox, and the recipient, idle, was never told
   anything. A message then waited in `stored` until somebody typed at the
   chat by hand. The wake pass runs on every tool event before the calling
   session is resolved, because the hook that fires belongs to whichever chat
   acted and the waiting messages are usually somebody else's. A recipient
   with no recorded socket is skipped, not guessed at.
10. `sent` names the readers, not only their number, and marks the ones that
   share only a shared room with the sender. A broadcast into a shared room
   reaches every member, so "3 read it" cannot say whether any of the three
   was the agent the message was about; one such read came from an agent that
   had joined the room for unrelated work and read a message addressed to
   everyone. `shared_only` is a hint, not a verdict: in the cross-repository
   case it is the normal shape, and only the sender knows who was a bystander.
   Nothing in the store distinguishes a bystander from an intended recipient,
   so the tool reports the shape rather than guessing at a flag.
11. A nudge names the rooms its messages came through, and an empty
   `msg_recv` says why it is empty. Both are the same defect: an answer that
   is correct but indistinguishable from a different, wrong one. The nudge
   used to name the session's first room, which for a cross-repository
   message is almost never where it arrived; `msg_recv` returning nothing
   used to be the same output for a caught-up session and a session in the
   wrong room. The rooms named are intersected with the reader's own
   membership, and past two rooms the line counts the rest.
12. A session id that names nothing is an error on every read, and the error
   names the id the caller probably meant. An unknown id and an empty inbox
   are both zero rows, so without the check the wrong id is invisible: an
   agent passed its harness key to `msg_recv` for weeks and concluded nothing
   had been sent to it while nineteen messages piled up. The harness key is
   the usual mistake, because the harness puts it in the environment and it
   looks like an identity; it is the join key, not the session id. Tools that
   resolve a room from a session refuse rather than fall back to the current
   directory, or a mistyped id writes into the wrong room.
13. Liveness is exact only on Linux, which is the only supported platform with
   `/proc`. On Windows and macOS the pid, start time and process environment
   are all unavailable without a platform-specific syscall, so liveness falls
   back to "is the pid running" and duplicate rows cannot be tied back to
   their chat. The fallback is weaker in one direction: a restarted MCP
   server leaves the row's pid stale and the row can read as `gone` while the
   chat works. Nothing is guessed from it — a session with no pid is never
   pruned — and `prune` and `room_who` take the same fallback, so they still
   agree with each other.

## Ground rules for changes

- English inside the repository. Conversation with the author is Russian.
- No daemon. If a change needs one, it is the wrong change.
- No dependency copied from another agent-coordination project.
- Every claim in the docs must be true of the code. Update the docs in the
  same commit as the code.
- Tests must not touch `~/.agents-room`; use `AGENTS_ROOM_DB` or a temporary
  path.
