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

1. A room-level autonomy cap that an operator can lower. Today the cap is
   the maximum level; the schema already carries a per-session level.
2. A Codex and a Hermes wake adapter beyond the generic hook output.
3. Whether `hold` should have a per-turn cap lower than the harness default.
4. Release packaging: signed checksums and a `go install` path.

## Ground rules for changes

- English inside the repository. Conversation with the author is Russian.
- No daemon. If a change needs one, it is the wrong change.
- No dependency copied from another agent-coordination project.
- Every claim in the docs must be true of the code. Update the docs in the
  same commit as the code.
- Tests must not touch `~/.agents-room`; use `AGENTS_ROOM_DB` or a temporary
  path.
