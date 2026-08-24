package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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

  mk_search       - full-text search across the embedded KB
  mk_show         - retrieve one page by ID (returns body + frontmatter)
  mk_list         - list pages, optionally filtered (prefix/category/status/owner)
  mk_save_memory  - save a personal/team/global memory, searchable at once
                    (only when a collection declares a "memory:" store)

Every tool takes an optional "collection" argument; with several
collections mounted, each tool's description names them, so a client
discovers the set from the tool list it already fetches.

stdio is unauthenticated by construction — the process was started by
the one user it serves — so personal memories saved here land in a fixed
"local" namespace rather than one derived from a token.

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

  mk_search       - full-text search across the KB
  mk_show         - retrieve one page by ID (returns body + frontmatter)
  mk_list         - list pages, optionally filtered
  mk_save_memory  - save a personal/team/global memory (only when a
                    collection declares a "memory:" store)

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

mk_save_memory is gated the same way, on the write capabilities
(personal-write / team-write / global-write) rather than on read: a
caller holding none of them anywhere is not offered the tool at all. A
personal memory's namespace comes from the verified token's subject and
issuer — there is no argument that could name anybody else. A team or
global memory a caller may not write is saved as a pending review
artifact under the store's _staging/ prefix, which is never indexed or
served. See docs/design/memory.md.

With NO auth: block configured the server is unauthenticated and every
mounted collection is readable by any caller — the same posture as
'mcp serve'. Bind loopback (the default) or put a gateway in front.

A collection whose content-source.yaml entry carries a "refresh:" block
is re-checked while the server runs: a metadata-only probe every
interval, and — only when the GCS object generation or prefix
fingerprint actually moved — a re-resolve, an off-request-path index
rebuild and an atomic swap. Queries keep being served throughout, from
the previous snapshot until the new one is complete. A "refresh:" block
under a collection's "memory:" is the same thing for a shared GCS memory
store, and is what makes several replicas converge on each other's
writes. SIGHUP runs every configured refresh immediately. See
docs/design/hot-reload.md.

An "observability:" block in content-source.yaml (or the standard OTEL_*
environment variables) turns on OpenTelemetry tracing and optional OTLP
export: one mk_search then becomes one trace spanning the HTTP request,
OIDC verification, the authorization decision, the tool call, the search
and any GCS or memory work underneath it, and the access log gains
matching trace_id/span_id. With no block and no OTEL_* variable nothing
is constructed at all — no spans, no exporter, no socket — and /metrics
and the JSON logs are exactly what they were. Spans carry counts,
durations and closed-set outcomes only: never a query, a page ID, a
collection name, a bucket, a token or a subject. A collector that is
down never affects a request, /readyz or shutdown. See
docs/design/observability.md.

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
				Observability:                 activeObservability,
				// This process runs exactly one hosted server, so it is the
				// one that may own the OpenTelemetry globals — which is what
				// makes the Google Cloud Storage client's own instrumentation
				// join meerkat's traces instead of emitting nowhere. A test
				// binary or an embedding host runs several and sets neither.
				SetOTelGlobals: true,
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
				go watchReloadSignal(ctx, s, cmd.ErrOrStderr())
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

// watchReloadSignal turns SIGHUP into an immediate reconciliation cycle
// for every collection with a `refresh:` block.
//
// SIGHUP rather than an admin HTTP endpoint, deliberately. An endpoint
// would be a new mutating surface that has to be authenticated (the
// operational endpoints beside it are all unauthenticated by design, and
// a reload trigger emphatically cannot join them), rate-limited (it can
// be made to hammer a bucket), and reasoned about for every deployment
// topology. A signal is authorized by the operating system: you can send
// it if you can already signal the process, which is strictly less
// access than being able to restart it — the thing this feature exists
// to avoid needing.
//
// It reaches the SAME code the scheduled loops use (HostedServer.Reload
// -> Controller.ReloadNow -> Target.Reconcile), so a manual reload
// cannot skip the staging discipline, the generation preconditions or
// the atomic swap, and cannot run concurrently with a scheduled cycle:
// the collection's reload slot refuses the second caller.
func watchReloadSignal(ctx context.Context, s *mcp.HostedServer, w io.Writer) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if err := s.Reload(ctx); err != nil {
				// Not fatal: a failed reload leaves the last known-good
				// content serving, which is the whole contract.
				fmt.Fprintf(w, "meerkat: reload failed (still serving the last known-good content): %v\n", err)
				continue
			}
			fmt.Fprintln(w, "meerkat: reload complete")
		}
	}
}
