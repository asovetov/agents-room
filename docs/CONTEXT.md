# Context for a new session

Read this first. It records what is decided, what is open, and where to start.

## What this project is

`agents-room` is a coordination layer for AI coding agents working in the
same codebase from different terminals and different harnesses. It provides
project rooms, session presence, file leases, drift detection and addressing
by file or tag, over MCP.

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
3. Release packaging: the tag workflow and `make install` exist; checksums
   are unsigned (no GPG key yet).
4. Automatic lease renewal. `lease_renew` exists, but nothing calls it on a
   schedule yet; a long edit can still outlive its TTL.
5. Automatic pruning. `agents-room prune` exists, but no timer runs it.

## Ground rules for changes

- English inside the repository. Conversation with the author is Russian.
- No daemon. If a change needs one, it is the wrong change.
- No dependency copied from another agent-coordination project.
- Every claim in the docs must be true of the code. Update the docs in the
  same commit as the code.
- Tests must not touch `~/.agents-room`; use `AGENTS_ROOM_DB` or a temporary
  path.
