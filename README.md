# agents-room

A coordination layer for AI coding agents working in the same codebase from
different terminals and different harnesses.

You run five agent sessions on one large project. Each one edits files, makes
decisions, and moves on. Nothing tells session two that session one is already
in `src/mavlink/`. Nothing tells the agent in the other repository that the
wire format changed. You become the courier, copying messages between windows.

agents-room answers three questions the harnesses do not answer:

- **Who is here?** Which sessions are working in this project, on what, right now.
- **Who is touching this file?** Leases on paths, with expiry and stale-holder detection.
- **Is my read still valid?** Drift warnings when a file changed under you.

It also delivers messages addressed by *what* rather than *who*:
`file:src/mavlink/**` reaches whoever holds those files, `tag:protocol`
reaches everyone working on the protocol.

## What it is not

- Not a chat platform. Lasting decisions belong in your long-term memory tool.
- Not a replacement for a harness's own cross-session messaging. Where the
  harness can reach a peer natively, agents-room adds the part it cannot do:
  leases, presence, and addressing by file.
- Not a multi-machine service. One machine, one store.

## How it works

There is no daemon. The shared state is a SQLite database in WAL mode at
`~/.agents-room/room.db`, and every participant is a short-lived process that
opens it directly.

```
   Claude Code        Codex           Hermes        (any MCP client)
       │                │                │
   MCP + hook       MCP + hook          MCP
       └────────────────┴────────────────┘
                        │
              ~/.agents-room/room.db
              rooms · sessions · messages
              leases · drift · events
```

Why no daemon: no port to manage, no orphan process, a crashed agent cannot
take the shared state with it, and rooms across related repositories work
without extra plumbing.

Concurrency is SQLite's job. Leases are claimed inside `BEGIN IMMEDIATE`, so
two agents claiming the same file cannot both win.

## Wake signals

A recipient is told that messages are waiting; it fetches the body itself.

```
[agents-room] 2 unread in room "repo:gcs"; call msg_recv
```

The body stays in the store and arrives as a tool result. This keeps context
small and makes the delivery state honest: `read` means the recipient saw the
content, not that a notification was emitted.

For Claude Code the signal is posted to the session's own inbox socket, which
wakes an idle session without approval. For other harnesses the hook falls
back to their event contract, and for harnesses with no wake lever the
message waits as `undeliverable` until the agent pulls it.

## Autonomy

Each session runs at one of three levels, negotiated with the operator:

| Level | Name | Behaviour |
|---|---|---|
| 0 | `notify` | one-line signal, never holds the end of a turn |
| 1 | `hold` | holds the end of a turn until unread is fetched |
| 2 | `wake` | as level 1, and may start a turn in an idle session |

Level 1 suits heavy contention or a long important task. Level 2 suits an
operator who stepped away. Level 0 suits an operator sitting next to the
machine. `room_who` shows every session's effective level.

## Install

```bash
go build -o bin/agents-room      ./cmd/agents-room
go build -o bin/agents-room-mcp  ./cmd/agents-room-mcp
go build -o bin/agents-room-hook ./cmd/agents-room-hook
```

Linux, macOS and Windows on amd64 and arm64. No cgo, no external runtime.
Setup per harness is in [`docs/harness-setup.md`](docs/harness-setup.md).

## Use

```bash
agents-room join --label "MAVLink dialect" --tag protocol --autonomy 1 --key "$SESSION_ID"
agents-room who
agents-room cap --set 1
agents-room lease claim --session <id> --path src/mavlink/wire.go --ttl 600
agents-room send  --session <id> --to file:src/mavlink/** --subject "dialect" --body "..."
agents-room recv  --session <id>
agents-room watch
```

The same operations are MCP tools: `room_join`, `room_leave`, `room_who`,
`room_set_autonomy_cap`, `msg_send`, `msg_recv`, `msg_ack`, `msg_unread`,
`lease_claim`, `lease_renew`, `lease_release`, `lease_check`, `lease_list`,
`note_read`, `drift_check`. The full reference is in
[`docs/protocol.md`](docs/protocol.md).

## Status

Early. The store, rooms, sessions, messages, leases, drift, the CLI, the MCP
server, schema migrations and the Claude Code and Hermes wake paths work and
are tested, including an end-to-end suite that runs the real binaries as
separate processes. See
[`docs/plan.md`](docs/plan.md) for milestones and what is still open.

## License

GPLv3. Free to use, modify and redistribute under the same terms. This
project will not be sold.
