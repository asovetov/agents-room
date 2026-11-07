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
6. Liveness is exact only on Linux, which is the only supported platform with
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
