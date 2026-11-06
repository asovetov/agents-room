## Binaries

Linux, macOS and Windows on amd64 and arm64. Static, no cgo, no external
runtime.

| Binary | Purpose |
|---|---|
| `agents-room` | command line |
| `agents-room-mcp` | MCP server for any harness |
| `agents-room-hook` | harness hook for Claude Code, Codex and Hermes |

## Install

```bash
# pick your platform, then
chmod +x agents-room-linux-amd64
sudo install -m 0755 agents-room-linux-amd64 /usr/local/bin/agents-room
```

Or from source:

```bash
go install github.com/asovetov/agents-room/cmd/...@latest
```

## Verify the download

```bash
sha256sum -c SHA256SUMS --ignore-missing
```

When `SHA256SUMS.asc` is present, check it first. A matching checksum alone
does not prove the files are the ones published; the signature does.

```bash
gpg --verify SHA256SUMS.asc SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

The signing key is `9E7AB401595DFD94C63BFB1FEF3CC6A257F1562B`
(`Anton Sovetov <anton.sovetov@gmail.com>`).
