# Harness setup

agents-room speaks MCP, so any MCP client can use the tools. Wake signals
are per-harness and optional: without one, the agent pulls with `msg_recv`.

## Install

Build the one binary and put it on `PATH`:

```bash
go build -o bin/agents-room ./cmd/agents-room
```

The same binary serves MCP (`agents-room mcp`) and runs harness hooks
(`agents-room hook`), so there is nothing else to install.

Linux, macOS, Windows on amd64 and arm64 build from the same source with no
cgo and no external runtime.

## Claude Code

### MCP server

Add to `~/.claude.json` or the project `.mcp.json`:

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

`args: ["mcp"]` is required. The binary without a subcommand prints its
usage and exits, so a registration that omits it starts a server that never
answers.

### Hooks

The hook binary joins the room at session start, leaves at session end, and
posts a one-line nudge to the session's own inbox socket when messages are
waiting.

`~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=SessionStart --autonomy=1",
        "timeout": 10 } ] }
    ],
    "SessionEnd": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=SessionEnd",
        "timeout": 5 } ] }
    ],
    "Stop": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=Stop",
        "timeout": 10 } ] }
    ],
    "PostToolBatch": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=PostToolBatch",
        "timeout": 10 } ] }
    ]
  }
}
```

Why the socket and not `additionalContext`: Claude Code treats a message
posted to a session's inbox socket by one of its own hook processes as an
own-child message and delivers it without approval. It wakes an idle session,
and it does not put the body into the context. The hook prints nothing to
stdout on success, so no duplicate context is injected.

`SessionStart` output goes straight into the model context, so the hook stays
silent there by design.

The `Stop` hook never blocks a Stop it caused itself. Claude Code sets
`stop_hook_active` on the Stop that continues a turn a previous Stop hook
held, and the hook stays silent on it whatever the autonomy level. Without
that check the agent can never finish: every block starts a turn whose Stop
is blocked again, and the chat spins until it is interrupted. A message that
was nudged but not fetched is offered again on the next Stop that is not a
continuation.

A join is keyed on the harness session id, so the five `SessionStart`
matchers (startup, resume, clear, compact, fork) all update one room
session instead of creating five.

### Autonomy in practice

- `--autonomy=0`: nudge only. The agent sees the line when it next runs a tool.
- `--autonomy=1`: the `Stop` hook holds the end of a turn and asks the agent
  to fetch unread messages. Good when contention is high or the task matters.
- `--autonomy=2`: as level 1, and an idle session is woken. Good when you are
  away and the agents are on their own.

Every nudge event also renews the leases the session holds. A session that is
running tools keeps what it claimed; one that stops working lets them lapse
after the TTL. `agents-room doctor` warns when a live lease has less than a
quarter of its life left, which is the sign that this hook is not running.

## Codex CLI

Codex hooks are stable and enabled by default since 0.124.0. Configure them
in `~/.codex/config.toml` or in `<project>/.codex/config.toml` (trusted
projects only):

```toml
[[hooks.SessionStart]]
[[hooks.SessionStart.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=SessionStart"
timeout = 15

[[hooks.SessionEnd]]
[[hooks.SessionEnd.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=SessionEnd"
timeout = 3

[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=UserPromptSubmit"
timeout = 10

[[hooks.PostToolUse]]
[[hooks.PostToolUse.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=PostToolUse"
timeout = 10

[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=Stop"
timeout = 30
```

| Event | Output | Effect |
|---|---|---|
| `SessionStart` | none | joins the room |
| `SessionEnd` | none | leaves the room and releases leases |
| `UserPromptSubmit` | JSON with `hookSpecificOutput.additionalContext` | adds the nudge to the turn |
| `PostToolUse` | same envelope | adds the nudge after a tool |
| `Stop` | `{"decision":"block","reason":"..."}` | holds the end of a turn, only at `hold` or above |

Plain stdout is only read as context on `SessionStart`, `SubagentStart` and
`UserPromptSubmit`; the tool events and `Stop` need the JSON envelope, which
is why the hook emits JSON for Codex rather than a bare line.

Two caveats that cost time if missed:

- Codex records a trust hash per hook and **will not run an untrusted or
  edited hook**. Approve it once with the `/hooks` command. `--dangerously-bypass-hook-trust`
  skips the check.
- On Windows the default hook shell is `cmd.exe`, not bash. The hook binary
  is a native executable so the command above works, but any shell syntax
  around it would need `commandWindows`.

MCP server registration:

```toml
# ~/.codex/config.toml
[mcp_servers.agents-room]
command = "agents-room"
args = ["mcp"]
```

Codex has no cross-session message bus of its own (`codex queue` writes into
one session's input queue but has no reply channel or discovery), so the
room is the only cross-session path there.

**Verified end to end on codex-cli 0.167.2:** `SessionStart`,
`UserPromptSubmit`, `PostToolUse` and `Stop` all fire, the session appears in
its repository room while the run is going, and `SessionEnd` removes it. That
last part is correct behaviour and easy to misread: check `who` *during* the
run, or watch the `session.join` / `session.leave` events.

There is no wake for Codex. `codex queue --thread <id> --message "…" --resume`
puts the message in a session's input queue, but resuming needs a running
app-server daemon; without one it prints "message was queued, but could not
resume thread". The daemon is not a configuration this project supports, so a
message waits for the agent's next turn.

Also for non-interactive runs: hooks are trusted per hash, so `codex exec`
needs `--dangerously-bypass-hook-trust` (or one `/hooks` approval in the
interactive client) before it will run them at all.

## Hermes

Hermes has shell hooks that can inject context and hold the end of a turn.

`agents-room doctor --fix` writes everything below into `~/.hermes/config.yaml`
— the six hooks, the MCP server, `hooks_auto_accept` and `--autonomy=2` — with
a backup beside the file and no reformatting of your own content. The
sections here are what it writes and why, for a hand-edited config.

```yaml
hooks:
  on_session_start:
    - command: agents-room hook --harness=hermes --event=on_session_start --autonomy=2
      timeout: 10
  on_session_end:
    - command: agents-room hook --harness=hermes --event=Stop --autonomy=2
      timeout: 10
  on_session_finalize:
    - command: agents-room hook --harness=hermes --event=on_session_finalize --autonomy=2
      timeout: 10
  pre_llm_call:
    - command: agents-room hook --harness=hermes --event=pre_llm_call --autonomy=2
      timeout: 10
  pre_verify:
    - command: agents-room hook --harness=hermes --event=pre_verify --autonomy=2
      timeout: 10
```

`on_session_end` carries `--event=Stop`, not `on_session_end`. The hook maps
that name to *leaving the room*, so wiring the natural name makes the session
drop out at the end of every turn. `--autonomy=2` is what lets a waiting
message start a turn while the chat is idle; without it the hooks run and
nothing is ever announced to a quiet session.

One caveat about where the session lands: Hermes reports the hook process's
directory as the session's, and that process lives in the install tree. The
hook therefore takes the real directory from Hermes' own session store
(`~/.hermes/state.db`), which is the same value `hermes sessions list` prints
as Workspace, and falls back to the payload only when that lookup has no
answer. A session started from a checkout joins that checkout's room.

The hook speaks Hermes's JSON wire protocol: a payload on stdin, a JSON
object on stdout.

| Event | Output | Effect |
|---|---|---|
| `on_session_start` | none | joins the room |
| `on_session_end` (`--event=Stop`) | none | announces waiting messages |
| `on_session_finalize` | none | leaves the room and releases leases |
| `pre_llm_call` | `{"context": "..."}` | appends the nudge to the user message |
| `pre_verify` | `{"action": "continue", "message": "..."}` | holds the end of a turn, only at `hold` or above |

`pre_llm_call` fires once per turn, so the nudge arrives with the next
message. `pre_verify` only fires when the agent edited code, which is
exactly the moment a lease or a message is most likely to matter.

Hermes also accepts the MCP server for the pull path:

```yaml
mcp_servers:
  agents-room:
    command: agents-room
    args: ["mcp"]
```

Hermes asks for consent the first time it sees a `(event, command)` pair.
Set `hooks_auto_accept: true` in the same config only if you accept every
hook in it.

The hook binary must be on the `PATH` Hermes inherits. `go install` puts it
in `$(go env GOPATH)/bin`, which is often not on `PATH`; install into a
directory that is, for example:

```bash
GOBIN=~/.local/bin go install github.com/asovetov/agents-room/cmd/agents-room@latest
```

Verified against Hermes 0.22.5 through its own shell-hook dispatcher: join,
`pre_llm_call` context injection, `pre_verify` holding the turn at `hold`
level, silence after `msg_recv`, and leave on `on_session_finalize`.

### Waking an idle Hermes session

By default a Hermes session is not woken: a message waits `stored` and is
announced on the agent's next turn, because Hermes sets no inbox socket and
there is no signal channel to use.

What can be woken is a session created by `hermes -z` or by the gateway: those
are the ones whose hooks register them in a room. An interactive `hermes chat`
session has been reported not to join at all — its `on_session_start` does not
run — so treat the TUI as a client with no room presence until that is
confirmed and fixed. This has not been verified here; check `agents-room who`
while the TUI is open before relying on it.

There is a way to start the turn from the room, and it is **off unless you
turn it on**:

```bash
echo 'hermes=cli' > ~/.agents-room/wake
```

With that file, a session at autonomy 2 and with a harness key is resumed with
`hermes --resume <key> -z "<nudge>"`. `doctor` reports whether it is on and
never turns it on for you; delete the file to turn it off again.

**What you are turning on.** `hermes -z` sets `HERMES_YOLO_MODE=1`, so the
turn runs commands without asking. It is started by a message another agent
sent. That is a materially different power from leaving a note, which is why
it is a deliberate line in a file rather than a default. One wake is emitted
per session per minute, not per message.

## Any other MCP client

Register `agents-room mcp` as a stdio server. The agent calls `room_join`
once, then `msg_recv` when it wants to check in.

## Shared rooms by default

A shared room is what lets an agent in one repository reach an agent in
another. Requiring every agent to remember `--also` means, in practice,
nobody uses it, so the hook discovers the rooms instead.

Two sources, both optional:

```bash
# once, in your shell profile: applies to every repository on this machine
export AGENTS_ROOM_SHARED=storefront
```

```text
# <repository>/.agents-room/shared-rooms: travels with the checkout
# one room per line, # starts a comment
storefront
```

The `shared:` prefix is added when missing. Duplicates are removed, and a
missing file means no shared rooms rather than an error.

`SessionStart` joins the repository room and every discovered shared room.
Passing `--room` explicitly to the hook turns discovery off for that session,
which is how to run one agent in isolation.

Discovery covers the hook path. An agent that joins through the CLI or the
MCP tool passes `--also` or `extra_rooms` itself, because there is no
environment for it to read. The prefix is added there too, so
`--also storefront` and `AGENTS_ROOM_SHARED=storefront` land in one room
rather than two that look alike.

## Keeping it current

```bash
agents-room update --check
agents-room update
```

The updater verifies the release signature against the key embedded in the
binary before it replaces anything, and migrates the harness configuration
in the same run. Running `agents-room doctor` afterwards reports any file
that still names an old command.

## Verifying the setup

```bash
agents-room path            # which database is in use
agents-room who             # who is in the current repository room
agents-room watch --once    # sessions, leases, unread counts
```

If `agents-room who` shows a session but `watch` shows no unread, the room is
working and there is simply nothing to deliver.
