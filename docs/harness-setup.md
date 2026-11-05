# Harness setup

agents-room speaks MCP, so any MCP client can use the tools. Wake signals
are per-harness and optional: without one, the agent pulls with `msg_recv`.

## Install

Build the three binaries and put them on `PATH`:

```bash
go build -o bin/agents-room      ./cmd/agents-room
go build -o bin/agents-room-mcp  ./cmd/agents-room-mcp
go build -o bin/agents-room-hook ./cmd/agents-room-hook
```

Linux, macOS, Windows on amd64 and arm64 build from the same source with no
cgo and no external runtime.

## Claude Code

### MCP server

Add to `~/.claude.json` or the project `.mcp.json`:

```json
{
  "mcpServers": {
    "agents-room": {
      "command": "agents-room-mcp"
    }
  }
}
```

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
        "command": "agents-room-hook --event=SessionStart --autonomy=1",
        "timeout": 10 } ] }
    ],
    "SessionEnd": [
      { "hooks": [ { "type": "command",
        "command": "agents-room-hook --event=SessionEnd",
        "timeout": 5 } ] }
    ],
    "Stop": [
      { "hooks": [ { "type": "command",
        "command": "agents-room-hook --event=Stop",
        "timeout": 10 } ] }
    ],
    "PostToolBatch": [
      { "hooks": [ { "type": "command",
        "command": "agents-room-hook --event=PostToolBatch",
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

### Autonomy in practice

- `--autonomy=0`: nudge only. The agent sees the line when it next runs a tool.
- `--autonomy=1`: the `Stop` hook holds the end of a turn and asks the agent
  to fetch unread messages. Good when contention is high or the task matters.
- `--autonomy=2`: as level 1, and an idle session is woken. Good when you are
  away and the agents are on their own.

## Codex CLI

Codex has `SessionStart`, `PostToolUse`, `Stop` and `PreCompact` hooks. Use
the same hook binary with `--harness=codex`; it falls back to stdout JSON for
`PostToolBatch` and `Stop` because Codex has no inbox socket.

```toml
# ~/.codex/config.toml
[mcp_servers.agents-room]
command = "agents-room-mcp"
```

## Hermes

Hermes is pull-only until an adapter exists. Register the MCP server and let
the agent call `msg_recv` at the start of a task.

## Any other MCP client

Register `agents-room-mcp` as a stdio server. The agent calls `room_join`
once, then `msg_recv` when it wants to check in.

## Verifying the setup

```bash
agents-room path            # which database is in use
agents-room who             # who is in the current repository room
agents-room watch --once    # sessions, leases, unread counts
```

If `agents-room who` shows a session but `watch` shows no unread, the room is
working and there is simply nothing to deliver.
