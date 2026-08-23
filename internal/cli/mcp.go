package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/mcp"
)

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP (Model Context Protocol) server",
		Long: `Manage MCP servers exposing the meerkat KB.

Two transports serve the identical tool set:

  mcp serve       stdio — spawned by a local MCP client, no auth
  mcp serve-http  Streamable HTTP — hosted, concurrent, OIDC-authenticated

Wire the stdio server into OpenCode by adding to
~/.config/opencode/opencode.json:

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
	cmd.AddCommand(newMCPServeCmd(), newMCPServeHTTPCmd())
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

func newMCPServeHTTPCmd() *cobra.Command {
	var (
		host           string
		port           int
		endpointPath   string
		authConfigPath string
		stateful       bool
		trustProxyHost bool
	)
	cmd := &cobra.Command{
		Use:   "serve-http",
		Short: "Serve the meerkat KB tools over MCP Streamable HTTP with OIDC auth",
		Long: `Run a hosted Model Context Protocol server on the Streamable HTTP
transport. It exposes the same tools as 'mcp serve':

  mk_search  - full-text search across the KB
  mk_show    - retrieve one page by ID (returns body + frontmatter)
  mk_list    - list pages, optionally filtered

Endpoints:

  /mcp                                    MCP Streamable HTTP (POST/GET/DELETE)
  /.well-known/oauth-protected-resource   RFC 9728 metadata (no auth)
  /livez                                  liveness probe (no auth)
  /readyz                                 readiness: content + index health (no auth)
  /metrics                                Prometheus metrics (no auth)

Authentication and authorization are configured by an "auth:" block in
content-source.yaml, or by a standalone file passed with --auth-config:

  auth:
    resource: https://mcp.example.com/mcp
    providers:
      - issuer: https://login.microsoftonline.com/<tenant>/v2.0
        audience: api://meerkat
        claims: { groups: groups, email: preferred_username, tenant: tid }
    rules:
      - name: sre
        groups: [sre]
        collections: [runbooks]
        capabilities: [read]

With providers configured, every request to /mcp must carry a verified
OIDC bearer token; one that doesn't gets 401 with a WWW-Authenticate
header pointing at the metadata endpoint. Each caller then sees ONLY
the collections their rules grant 'read' on — the rest are invisible,
not merely denied: they are absent from tool descriptions, from search
and list results, from the collection named in an error message, and
from show's ambiguity resolution.

With NO auth: block configured the server is unauthenticated and every
mounted collection is readable by any caller — the same posture as
'mcp serve'. Bind loopback (the default) or put a gateway in front.

The server has no TLS of its own; terminate TLS at a reverse proxy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			auth := activeAuth
			if authConfigPath != "" {
				loaded, err := contentsource.LoadAuthFile(authConfigPath)
				if err != nil {
					return err
				}
				if auth != nil {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"warning: --auth-config overrides the auth: block in content-source.yaml")
				}
				auth = loaded
			}

			cfg := mcp.HostedConfig{
				Addr:                          mcp.ResolveListenAddr(host, port),
				EndpointPath:                  endpointPath,
				Collections:                   registry(),
				Auth:                          auth,
				Version:                       version,
				Stateful:                      stateful,
				DisableDNSRebindingProtection: trustProxyHost,
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			err := mcp.ServeStreamableHTTP(ctx, cfg, func(s *mcp.HostedServer) {
				mode := "none (every mounted collection is public to any caller)"
				if s.AuthEnabled() {
					mode = "oidc"
				}
				fmt.Fprintf(cmd.ErrOrStderr(),
					"meerkat hosted MCP serving on %s%s (auth: %s)\n",
					s.Addr(), s.EndpointPath(), mode)
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("mcp serve-http: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Bind host (use 0.0.0.0 to listen on all interfaces)")
	cmd.Flags().IntVar(&port, "port", 4005, "Bind port")
	cmd.Flags().StringVar(&endpointPath, "path", mcp.DefaultEndpointPath, "Path the MCP Streamable HTTP endpoint is mounted at")
	cmd.Flags().StringVar(&authConfigPath, "auth-config", "",
		"Path to a standalone YAML policy file with a top-level auth: block. "+
			"Overrides the auth: block in content-source.yaml.")
	cmd.Flags().BoolVar(&stateful, "stateful", false,
		"Keep per-session state in this process instead of accepting any well-formed session ID. "+
			"Requires sticky routing when more than one replica sits behind a load balancer.")
	cmd.Flags().BoolVar(&trustProxyHost, "trust-proxy-host", false,
		"Disable DNS-rebinding protection (which rejects loopback requests whose Host header is not "+
			"a localhost value). Only for a same-host reverse proxy that preserves the original Host "+
			"header; prefer rewriting Host at the proxy instead.")
	return cmd
}
