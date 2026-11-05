// Command agents-room-mcp serves agents-room over MCP on stdio.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/asovetov/agents-room/internal/hub"
	"github.com/asovetov/agents-room/internal/mcpserver"
)

func main() {
	h, err := hub.Open()
	if err != nil {
		log.Fatalf("agents-room-mcp: %v", err)
	}
	defer h.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := mcpserver.New(h)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("agents-room-mcp: %v", err)
	}
}
