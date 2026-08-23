package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/mcp"
)

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP (Model Context Protocol) server",
		Long: `Manage MCP servers exposing the meerkat KB.

Wire into OpenCode by adding to ~/.config/opencode/opencode.json:

  {
    "mcp": {
      "meerkat": {
        "type": "local",
        "command": ["mk", "mcp", "serve"],
        "enabled": true
      }
    }
  }`,
	}
	cmd.AddCommand(newMCPServeCmd())
	return cmd
}

func newMCPServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the meerkat KB tools over MCP/stdio",
		Long: `Run a Model Context Protocol server on stdio. Exposes:

  mk_search  - full-text search across the embedded KB
  mk_show    - retrieve one page by ID (returns body + frontmatter)
  mk_list    - list pages, optionally filtered (prefix/category/status/owner)

Every tool takes an optional "collection" argument; with several
collections mounted, each tool's description names them, so a client
discovers the set from the tool list it already fetches.

Designed to be spawned by an MCP client (OpenCode, Claude Desktop, etc.).
The server runs until stdin closes or it receives SIGINT/SIGTERM.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := mcp.ServeStdio(ctx, registry()); err != nil {
				return fmt.Errorf("mcp serve: %w", err)
			}
			return nil
		},
	}
}
