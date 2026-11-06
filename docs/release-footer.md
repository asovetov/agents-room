---

## Install

Pick your platform from the assets, then:

```bash
chmod +x agents-room-linux-amd64
sudo install -m 0755 agents-room-linux-amd64 /usr/local/bin/agents-room
```

Or from source:

```bash
go install github.com/asovetov/agents-room/cmd/...@latest
```

The hook binary must be on the `PATH` the harness inherits; `GOBIN=~/.local/bin`
is a directory that usually already is.

## Verify the download

A matching checksum alone does not prove the files are the ones published;
the signature does.

```bash
gpg --verify SHA256SUMS.asc SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

The signing key is `9E7AB401595DFD94C63BFB1FEF3CC6A257F1562B`
(`Anton Sovetov <anton.sovetov@gmail.com>`).

## What is in the archive

One binary per platform. It is also the MCP server and the harness hook:

| Command | Purpose |
|---|---|
| `agents-room` | the command line |
| `agents-room mcp` | MCP server on stdio, for any harness |
| `agents-room hook` | one harness hook event |

Linux, macOS and Windows on amd64 and arm64. Static, no cgo, no external
runtime.
