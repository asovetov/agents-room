package mcpserver

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/asovetov/agents-room/internal/hub"
)

// Serve runs the MCP server over stdio until the context is cancelled.
//
// It is called as `agents-room mcp` from a harness configuration.
func Serve() error {
	h, err := hub.Open()
	if err != nil {
		return err
	}
	defer h.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return New(h).Run(ctx, &mcp.StdioTransport{})
}
