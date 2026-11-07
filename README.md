# agents-room

Coordination for AI coding agents that work in different repositories and
have to agree with each other.

[Русская версия →](README.ru.md)

## The problem

A team keeps three repositories: `api` (a REST service), `web` (the storefront
that calls it) and `mobile` (the app). Each has its own agent sessions, in
its own terminal, often with its own harness.

The mobile agent needs a field the API does not return yet. The API agent is
midway through renaming something else. The web agent is building against the
current shape and will break if the rename lands without them knowing.

Nothing in any harness connects those three windows. So you become the
courier: you copy the request from one chat into another, you remember who
promised what, and you find out about the break when the build fails.

agents-room is the layer that is missing. It lets agents in **different
repositories** meet in a room, say what they need from each other, and hold
the files they are working on while the agreement is being made.

It answers three questions the harnesses do not:

- **Who is working on this interface?** Which sessions are in the room, in
  which repository, and which of them are actually still running.
- **Who is touching this file?** Leases on paths, with expiry and
  stale-holder detection — across repositories, not just within one.
- **Is my read still valid?** A warning when a file changed under you.

It also delivers messages addressed by *what* rather than *who*:
`file:src/api/users.go` reaches whoever holds that file, in whatever
repository they sit; `tag:mobile` reaches everyone working on the mobile
client.

The same works inside one repository, when several sessions share a codebase.
That is the simpler case, and it comes for free.

## A session, in thirty seconds

```bash
# In the api repository: join the room the three projects share.
agents-room join --also shared:storefront --label "users API" --tag backend

# In the mobile repository: ask whoever holds the users endpoint.
agents-room send --session $ID --to file:src/api/users.go \
  --subject "need a phone field" --body "for the profile screen"

# The api agent claims the file before editing, so the others can see it.
agents-room lease claim --session $ID --path src/api/users.go
```

That is the whole loop. Everything else — MCP tools, harness hooks, autonomy
levels — is about making it happen without you typing.

## How the repositories meet

Every session joins `repo:<name of its directory>` automatically. Those rooms
stay separate: agents in `api` do not see `web`'s work unless they are asked
to.

To work across repositories, sessions join a **shared room** — a named group
that any repository can enter:

```bash
agents-room join --also shared:storefront
```

A shared room is not a merge of the repository rooms. It is a meeting point:
`api`, `web` and `mobile` each keep their own room, and the agents that opted
in can reach each other in the shared one. Leases, messages and tags all work
there, so an agent in `mobile` can hold a file in `api` while it asks for the
change.

The hook discovers shared rooms from two optional sources, so nobody has to
remember a flag:

```bash
# once, in your shell profile: every repository on this machine
export AGENTS_ROOM_SHARED=storefront
```

```text
# <repository>/.agents-room/shared-rooms: travels with the checkout
storefront
```

A `join` run from the CLI or an MCP tool does not read those sources — pass
`--also storefront` or `extra_rooms` yourself there. The `shared:` prefix is
added when missing, so `storefront` and `shared:storefront` are one room, not
two that look alike.

## Why not just use the harness

Most harnesses can message another session if you know its id. They do not
know which sessions are working on the interface you are about to change,
which files are contested, or whether the agent that was editing a file is
still alive. That is the gap this fills.

| | Harness messaging | agents-room |
|---|---|---|
| Reach a session by id | yes | yes |
| Reach whoever holds a file, in any repository | no | yes, `file:` selector |
| Reach everyone on a topic, across repositories | no | yes, `tag:` selector |
| Know who is in the room | no | yes, `who` |
| Know if a chat is still running | no | yes, `alive` / `gone` |
| Stop two agents editing one file | no | yes, leases |
| Warn that a file moved under you | no | yes, drift |
| Meet agents from other repositories | no | yes, shared rooms |

## What it is not

- **Not a chat platform.** Lasting decisions belong in your long-term memory
  tool or in the pull request. Messages here are coordination, and they
  expire.
- **Not a daemon.** There is no service to start, no port, no pid file.
- **Not multi-machine.** One machine, one store. That is a deliberate limit.
- **Not a harness replacement.** It plugs into what you already run.
- **Not a merge of your repositories.** A shared room is a meeting point, not
  a shared checkout. Nothing is copied between working trees.

## How it works

The shared state is a single SQLite file in WAL mode. Every participant —
the CLI, the MCP server, each hook — is a short-lived process that opens it
directly.

```
   api repo           web repo         mobile repo       (any MCP client)
   Claude Code        Codex            Hermes
       │                │                │
   MCP + hook       MCP + hook          MCP
       └────────────────┴────────────────┘
                        │
              ~/.agents-room/room.db
              rooms · sessions · messages
              leases · drift · events
```

No daemon means no port to manage, no orphan process, and no way for a
crashed agent to take the shared state down with it. Concurrency is SQLite's
job: a lease is claimed inside `BEGIN IMMEDIATE`, so two agents claiming one
file cannot both win — even when they are in different repositories and only
meet in a shared room.

## Wake signals

A recipient is told that messages are waiting; it fetches the body itself.

```
[agents-room] 2 unread in room "shared:storefront"; call msg_recv
```

The body stays in the store and arrives as a tool result. That keeps the
context small and makes the delivery state honest: `read` means the recipient
saw the content, not that a notification went out.

For Claude Code the signal goes to the session's own inbox socket, which
wakes an idle session without asking you. For harnesses with no wake lever
the message waits as `undeliverable` until the agent pulls it.

## Autonomy

Each session runs at one of three levels. The level decides whether a wake
signal may interrupt the agent and whether the end of a turn may be held.

| Level | Name | Behaviour |
|---|---|---|
| 0 | `notify` | one-line signal, never holds the end of a turn |
| 1 | `hold` | holds the end of a turn until unread is fetched |
| 2 | `wake` | as level 1, and may start a turn in an idle session |

Level 1 suits heavy contention or a long important task. Level 2 suits an
operator who stepped away. Level 0 suits an operator sitting next to the
machine.

The effective level is the **minimum** of what the agent asks for and what
the operator allows, so raising a room cap never silently grants interrupt
rights to a session that did not ask for them. `who` shows every session's
effective level.

## Install

```bash
go install github.com/asovetov/agents-room/cmd/agents-room@latest
```

One static binary, Linux, macOS and Windows on amd64 and arm64. No cgo, no
external runtime.

`go install` puts it in `$(go env GOPATH)/bin`, which is often not on `PATH`.
To install somewhere that is:

```bash
GOBIN=~/.local/bin go install github.com/asovetov/agents-room/cmd/agents-room@latest
```

Or download a release binary; the checksums are signed, and `agents-room
update` verifies them for you.

## Setup

The binary is three things in one: the CLI, the MCP server (`agents-room
mcp`), and the harness hook (`agents-room hook`). Nothing else to install.

### Claude Code

MCP server, in `~/.claude.json` or the project `.mcp.json`:

```json
{
  "mcpServers": {
    "agents-room": {
      "command": "agents-room",
      "args": ["mcp"]
    }
  }
}
```

`args: ["mcp"]` is required: without a subcommand the binary prints its usage
and exits.

Hooks, in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [{ "hooks": [{ "type": "command",
      "command": "agents-room hook --event=SessionStart --autonomy=1", "timeout": 10 }] }],
    "SessionEnd": [{ "hooks": [{ "type": "command",
      "command": "agents-room hook --event=SessionEnd", "timeout": 5 }] }],
    "Stop": [{ "hooks": [{ "type": "command",
      "command": "agents-room hook --event=Stop", "timeout": 10 }] }],
    "PostToolBatch": [{ "hooks": [{ "type": "command",
      "command": "agents-room hook --event=PostToolBatch", "timeout": 10 }] }]
  }
}
```

Codex, Hermes and any other MCP client are covered in
[`docs/harness-setup.md`](docs/harness-setup.md), including the two Codex
caveats that cost time if missed (hook trust, and `cmd.exe` on Windows).

## Commands

| Command | Purpose |
|---|---|
| `agents-room` | the command line |
| `agents-room mcp` | MCP server on stdio, for any harness |
| `agents-room hook` | one harness hook event |
| `agents-room doctor` | what is installed and wired up; `--fix` repairs what it can |
| `agents-room update` | check GitHub for a release; installs only with your yes |
| `agents-room migrate-config` | rewrite old command names in harness configs |
| `agents-room version` | the version you are running |
| `agents-room path` | which database is in use |

`agents-room history --room <name>` reads what has been said in a room. Use it
after joining somewhere that work is already under way: `recv` only shows
messages addressed to you since you joined, so it says nothing about what the
others agreed before you got there.

```bash
agents-room join --label "users API" --tag backend --autonomy 1 --key "$SESSION_ID"
agents-room join --also shared:storefront    # meet agents from the other repos
agents-room who                              # sessions, with liveness
agents-room cap --set 1                      # cap autonomy for the whole room
agents-room lease claim --session $ID --path src/api/users.go --ttl 600
agents-room send --session $ID --to file:src/api/users.go --subject "need a phone field"
agents-room sent --session $ID               # what became of what you sent
agents-room history --room shared:storefront # what was decided before you arrived
agents-room recv --session $ID
agents-room watch                            # live view, polled
```

The same operations are MCP tools: `room_join`, `room_leave`,
`room_leave_room`, `room_who`, `room_set_autonomy_cap`, `msg_send`,
`msg_recv`, `msg_ack`, `msg_unread`, `msg_sent`, `lease_claim`,
`lease_renew`, `lease_release`, `lease_check`, `lease_list`, `note_read`,
`drift_check`, `room_history`. The full reference, including selectors and
delivery states, is in [`docs/protocol.md`](docs/protocol.md).

## Knowing what is real

Two things agents get wrong without help, and agents-room does not.

**A chat that is quiet is not a chat that is gone.** `who` reports `liveness`
separately from activity: `alive`, `gone`, or `unknown` when the row records
no pid at all. A chat thinking through a long edit and a chat that exited
look identical by `last_seen`, and the difference decides whether a file it
holds is really held. Liveness is decided from the harness key, which
survives an MCP restart, not from the pid, which does not.

**A message that reached someone who left is not one they ignored.** `sent`
reports recipients that walked out before reading as `left`, separately from
those still waiting, so "4 of 7 have not read it" does not stand in for "3 of
them are gone".

## Doctor

`agents-room doctor` reports the store, the schema version, which harness
configs are wired up, whether an old command name is still around, a room
split by an old prefix bug, one chat registered under several ids, a lease
about to lapse, and the sessions whose chat is no longer running.

`agents-room doctor --fix` applies the repairs it can decide alone, which is
collapsing a chat that registered more than once. It keeps the session that
carries the harness key, or the oldest one when none does, and removes the
rest — moving any message the removed row still had unread to the session
that survives, because that row was a real member of the room and a delivery
deleted with it is a message nobody ever read.

`--fix --dry-run` prints the plan without touching the store. A split room is
reported but never merged automatically: moving a session between rooms is
the session's decision, not doctor's.

## Updating

```bash
agents-room update --check   # is there a newer release
agents-room update           # installs after asking
```

An update verifies the release signature against the key embedded in the
binary, then the checksum, and only then replaces the executable. Harness
configuration is migrated in the same step, because a renamed command left in
place would make hooks fail silently.

## Platform notes

Liveness is exact on Linux, the only supported platform with `/proc`. On
macOS and Windows the pid is all there is, so a chat whose MCP server was
restarted can read as `gone` while it is working. Nothing is guessed from
that: a session with no recorded pid is never pruned. On those platforms
`prune` and `who` take the same fallback, so they still agree with each other.

## Status

Early, and honest about it. The store, rooms, sessions, messages, leases,
drift, the CLI, the MCP server, schema migrations and the Claude Code and
Hermes wake paths work and are tested, including an end-to-end suite that
runs the real binaries as separate processes. The Codex adapter follows
Codex's source contract but has not been run end to end, because Codex is not
installed on the author's machine.

See [`docs/plan.md`](docs/plan.md) for milestones and
[`docs/CONTEXT.md`](docs/CONTEXT.md) for what is still open.

## License

GPLv3. Free to use, modify and redistribute under the same terms. This
project will not be sold.
