package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
	"github.com/zegit-zoo/meerkat/internal/mcp"
	"github.com/zegit-zoo/meerkat/internal/retrieval"
	"github.com/zegit-zoo/meerkat/internal/traversal"
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

  mk_search       - full-text search across the loaded KB
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

A type: local collection with a "refresh:" block in content-source.yaml
is re-checked on its interval while the server runs, and its search index
is rebuilt when its pages change on disk. Without one, nothing polls.
(Object-store and memory refresh: blocks are followed by serve-http.)

Designed to be spawned by an MCP client (OpenCode, Claude Desktop, etc.).
The server runs until stdin closes or it receives SIGINT/SIGTERM.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			outcome, err := outcomeOptions(ctx)
			if err != nil {
				return err
			}
			if err := mcp.ServeStdioWith(ctx, registry(), outcome); err != nil {
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
		securityTxt    bool
		insecureNoAuth bool
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
  /.well-known/security.txt               meerkat's security contact, RFC 9116 (no auth;
                                          --security-txt=false turns it off)

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

Selected collections can be published to callers with NO token at all,
with an "anonymous: true" rule:

  rules:
    - name: public-handbook
      anonymous: true
      collections: [handbook]
      capabilities: [read]         # anonymous rules are read-only

Everything else still requires a verified token. An anonymous caller
sees exactly the published collections and nothing else — the rest stay
indistinguishable from collections this deployment never mounted — and
is offered no write tool and owns no personal memories. A token that is
present but expired, malformed or forged is still 401: it is NEVER
downgraded to anonymous access, because an expiry that silently degrades
into partial data is an outage nobody sees. Cannot be combined with
allow_unauthenticated.

With NO auth: block configured the server is unauthenticated and every
mounted collection is readable by any caller — the same posture as
'mcp serve'. Bind loopback (the default) or put a gateway in front: the
server REFUSES to start on a non-loopback address with no authentication
unless --insecure-no-auth is given (or the auth: block says
allow_unauthenticated: true). An auth: block that names no providers, or
carries a key meerkat does not know, is a startup error rather than a
silent "no auth".

A collection whose content-source.yaml entry carries a "refresh:" block
is re-checked while the server runs: a metadata-only probe every
interval, and — only when the GCS object generation or prefix
fingerprint actually moved — a re-resolve, an off-request-path index
rebuild and an atomic swap. Queries keep being served throughout, from
the previous snapshot until the new one is complete. A "refresh:" block
under a collection's "memory:" is the same thing for a shared GCS memory
store, and is what makes several replicas converge on each other's
writes. SIGHUP runs every configured refresh immediately, and rebuilds
the search index of every "type: local" collection from what is on disk
now: its pages are read live, but its index is built once, so a page
added after startup is only searchable after a SIGHUP (or a restart).
The rebuild is swapped in atomically; queries keep being served from
the old index until it is ready. See docs/design/hot-reload.md.

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

Under systemd socket activation (a .socket unit with one ListenStream=,
which sets LISTEN_FDS=1 and LISTEN_PID) the server serves on the socket
systemd passes in, and --host and --port are ignored. systemd then holds
the port across a restart: a client that connects while the service
restarts waits and is served, instead of being refused. See the README.

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
			warnSuppressedAuth(cmd, authConfigPath, auth)

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			outcome, err := outcomeOptions(ctx)
			if err != nil {
				return err
			}

			// Taken before the slow startup work, so the inherited socket
			// is claimed (and its variables cleared) before anything could
			// spawn a child. Until Serve, systemd's queued connections
			// simply wait.
			listener, err := mcp.ActivationListener()
			if err != nil {
				return fmt.Errorf("mcp serve-http: %w", err)
			}
			if listener != nil && (cmd.Flags().Changed("host") || cmd.Flags().Changed("port")) {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"warning: socket-activated by systemd; --host and --port are ignored")
			}
			bind := host
			if listener != nil {
				bind = listener.Addr().String()
			}
			if err := checkUnauthenticatedBind(bind, auth, insecureNoAuth); err != nil {
				if listener != nil {
					_ = listener.Close()
				}
				return err
			}

			cfg := mcp.HostedConfig{
				Outcome:                       outcome,
				Addr:                          mcp.ResolveListenAddr(host, port),
				Listener:                      listener,
				EndpointPath:                  endpointPath,
				Collections:                   registry(),
				Auth:                          auth,
				Version:                       version,
				Stateful:                      stateful,
				DisableDNSRebindingProtection: trustProxyHost,
				NoSecurityTxt:                 !securityTxt,
				Observability:                 activeObservability,
				// This process runs exactly one hosted server, so it is the
				// one that may own the OpenTelemetry globals — which is what
				// makes the Google Cloud Storage client's own instrumentation
				// join meerkat's traces instead of emitting nowhere. A test
				// binary or an embedding host runs several and sets neither.
				SetOTelGlobals: true,
			}

			err = mcp.ServeStreamableHTTP(ctx, cfg, func(s *mcp.HostedServer) {
				mode := "none (every mounted collection is public to any caller)"
				switch {
				case s.AuthEnabled() && s.AnonymousEnabled():
					mode = "oidc + anonymous access to the collections published by an 'anonymous: true' rule"
				case s.AuthEnabled():
					mode = "oidc"
				}
				activated := ""
				if s.SocketActivated() {
					activated = " (socket-activated)"
				}
				fmt.Fprintf(cmd.ErrOrStderr(),
					"meerkat hosted MCP serving on %s%s%s (auth: %s)\n",
					s.Addr(), s.EndpointPath(), activated, mode)
				sighup := notifyReload()
				go reloadOnSignal(ctx, s, cmd.ErrOrStderr(), sighup)
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
	cmd.Flags().BoolVar(&insecureNoAuth, "insecure-no-auth", false,
		"Allow binding a non-loopback address with no authentication configured. Without it the "+
			"server refuses to start in that state; use it only when a gateway in front authenticates every request.")
	cmd.Flags().BoolVar(&securityTxt, "security-txt", true,
		"Serve /.well-known/security.txt (RFC 9116) with meerkat's security contact, unauthenticated. "+
			"Turn it off when the host serves a security.txt of its own")
	return cmd
}

// checkUnauthenticatedBind refuses to serve with no authentication on an
// address other hosts can reach. bind is a host or a host:port (the
// address of a socket-activated listener). An auth: block with
// allow_unauthenticated: true is the explicit, in-file opt-in for a
// gateway-fronted deployment; --insecure-no-auth is the command-line one.
func checkUnauthenticatedBind(bind string, auth *authz.Config, insecureNoAuth bool) error {
	if auth != nil && auth.Enabled() {
		return nil
	}
	if insecureNoAuth || isLoopbackBind(bind) {
		return nil
	}
	return fmt.Errorf("mcp serve-http: refusing to serve on %q with no authentication: every mounted collection would be "+
		"readable by any caller that can reach it. Configure an auth: block (or --auth-config), bind a loopback "+
		"address, or pass --insecure-no-auth if a gateway in front authenticates every request", bind)
}

// isLoopbackBind reports whether bind (host or host:port) is a loopback
// address. An empty host means "default", which this command binds to
// loopback; "0.0.0.0" and "::" listen on every interface and are not.
func isLoopbackBind(bind string) bool {
	host := bind
	if h, _, err := net.SplitHostPort(bind); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// warnSuppressedAuth tells the operator when --kb-dir/MEERKAT_KB_DIR made
// this server ignore an auth: block that a content-source.yaml would
// otherwise have supplied: the server then runs with whatever --auth-config
// says, or with no authentication at all.
func warnSuppressedAuth(cmd *cobra.Command, authConfigPath string, auth *authz.Config) {
	if authConfigPath != "" || auth != nil {
		return
	}
	kbDir, _ := cmd.Flags().GetString("kb-dir")
	if kbdir.Resolve(kbDir) == "" {
		return
	}
	contentSource, _ := cmd.Flags().GetString("content-source")
	found, err := contentsource.LoadRuntimeAuth(contentSource)
	if err != nil || found == nil {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(),
		"warning: --kb-dir/MEERKAT_KB_DIR is set, so the auth: block in content-source.yaml is IGNORED and this "+
			"server runs without it; pass --auth-config to supply the policy, or unset --kb-dir/MEERKAT_KB_DIR")
}

// notifyReload registers for SIGHUP and returns the channel it arrives
// on. It registers synchronously, before the reload loop starts, so a
// SIGHUP sent right after the "serving" line is caught rather than
// killing the process — the default action for an unhandled SIGHUP. The
// registration lives for the rest of the process.
func notifyReload() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	return ch
}

// reloader is the part of the hosted server reloadOnSignal needs.
type reloader interface {
	Reload(ctx context.Context) error
}

// reloadOnSignal turns each SIGHUP into an immediate reconciliation cycle
// for every collection with a `refresh:` block, and an index rebuild for
// every `type: local` collection (#105).
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
func reloadOnSignal(ctx context.Context, s reloader, w io.Writer, ch <-chan os.Signal) {
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

// outcomeOptions opens mk_report_outcome's sinks from the runtime
// content-source.yaml: the traversal log named under
// observability.traversal_log (its HMAC key read from the environment
// variable the block names) and the intake store under intake:. Either
// may be absent; the tool then records telemetry only and says so.
func outcomeOptions(ctx context.Context) (mcp.OutcomeOptions, error) {
	var out mcp.OutcomeOptions
	if activeObservability != nil && activeObservability.TraversalLog != nil {
		log, err := traversal.Open(ctx, activeObservability.TraversalLog, traversal.NewS3Sink)
		if err != nil {
			return out, fmt.Errorf("observability.traversal_log: %w", err)
		}
		out.Log = log
	}
	registry().SetCache(activeCache, out.Log)
	idle := contentsource.DefaultSessionIdleTimeout
	if activeSessions != nil {
		idle = activeSessions.IdleTimeout
	}
	limits := registry().TreeLimits()
	if limits == (contentsource.Limits{}) {
		limits = contentsource.DefaultLimits
	}
	out.Sessions = retrieval.New(idle, retrieval.Limits{MaxHops: limits.MaxHops, MaxSteps: limits.MaxSteps, MaxAttempts: limits.MaxAttempts})
	if spec := activeIntake; spec != nil {
		store, err := spec.Open(ctx, "")
		if err != nil {
			return out, fmt.Errorf("intake: %w", err)
		}
		out.Intake = store
	}
	return out, nil
}
