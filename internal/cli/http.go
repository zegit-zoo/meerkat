package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	mhttp "github.com/zegit-zoo/meerkat/internal/http"
	"github.com/zegit-zoo/meerkat/internal/wellknown"
)

func init() {
	// Both servers publish /.well-known/security.txt; its Expires date is
	// fixed from the build date the release pipeline stamps into `date`.
	wellknown.SetBuildDate(date)
}

func newHTTPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "http",
		Short: "Run an HTTP/OpenAPI server",
		Long: `Serve the meerkat KB over HTTP for OpenWebUI tool servers and
similar clients. The endpoint surface mirrors MCP 1:1.`,
	}
	cmd.AddCommand(newHTTPServeCmd())
	return cmd
}

func newHTTPServeCmd() *cobra.Command {
	var (
		host        string
		port        int
		apiKey      string
		apiKeyFile  string
		securityTxt bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the meerkat KB tools over HTTP/JSON with bearer auth",
		Long: `Run an HTTP/OpenAPI server. Endpoints:

  POST /search        full-text search
  POST /show          retrieve one page (body + frontmatter)
  POST /list          enumerate pages with filters
  GET  /collections   enumerate the mounted collections
  GET  /openapi.json  schema (no auth)
  GET  /healthz       liveness (no auth)
  GET  /.well-known/security.txt
                      meerkat's security contact, RFC 9116 (no auth;
                      --security-txt=false turns it off)

/search, /show and /list take an optional "collection" field; omitted,
they span every mounted collection.

This server loads its collections once and never runs a "refresh:"
block, so GET /collections reports no refresh status and no
freshness. For a server that follows its sources, use
"mk mcp serve-http".

Authentication: all data endpoints require an Authorization: Bearer
header carrying the configured API key. Supply the key in a file
(--api-key-file, preferably mode 0600; surrounding whitespace is
trimmed) or in the MEERKAT_API_KEY env var; the env var wins if both
are set. --api-key also works, but a value on the command line is
visible to other local users in the process list. The server refuses to
start without a key — there is no anonymous mode — or with one shorter
than 16 characters; 'openssl rand -hex 32' makes a good one.

Register http://<host>:<port>/openapi.json with OpenWebUI as a Tool
Server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, warnings, err := resolveAPIKey(os.Getenv("MEERKAT_API_KEY"), apiKeyFile, apiKey)
			for _, w := range warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w)
			}
			if err != nil {
				return err
			}
			apiKey = key

			cfg := mhttp.Config{
				Addr:          mhttp.ResolveListenAddr(host, port),
				APIKey:        apiKey,
				Version:       version,
				Collections:   registry(),
				NoSecurityTxt: !securityTxt,
			}
			srv, err := mhttp.New(cfg)
			if err != nil {
				return fmt.Errorf("init http server: %w", err)
			}
			defer srv.Close()

			fmt.Fprintf(cmd.ErrOrStderr(),
				"meerkat http serving on %s (auth: bearer)\n", srv.Addr())

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := srv.ListenAndServe(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("http serve: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Bind host (use 0.0.0.0 to listen on all interfaces)")
	cmd.Flags().IntVar(&port, "port", 4004, "Bind port")
	cmd.Flags().BoolVar(&securityTxt, "security-txt", true,
		"Serve /.well-known/security.txt (RFC 9116) with meerkat's security contact, unauthenticated. "+
			"Turn it off when the host serves a security.txt of its own")
	cmd.Flags().StringVar(&apiKey, "api-key", "",
		"Static bearer token (min 16 characters). Visible to other local users in the process list: "+
			"prefer --api-key-file or MEERKAT_API_KEY.")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "",
		"Read the static bearer token from this file (min 16 characters; surrounding whitespace is trimmed). "+
			"Keep it mode 0600. MEERKAT_API_KEY, if set, takes precedence.")
	cmd.MarkFlagsMutuallyExclusive("api-key", "api-key-file")
	return cmd
}

// resolveAPIKey picks the API key from, in order of precedence, the
// MEERKAT_API_KEY environment variable, the --api-key-file file and the
// --api-key flag value. It returns warnings for the caller to print.
func resolveAPIKey(env, file, flagValue string) (key string, warnings []string, err error) {
	fromFile := ""
	if file != "" {
		fromFile, warnings, err = readAPIKeyFile(file)
		if err != nil {
			return "", warnings, err
		}
	}
	if flagValue != "" {
		warnings = append(warnings, "--api-key puts the key in the process list, visible to other local users; "+
			"use --api-key-file or MEERKAT_API_KEY instead")
	}
	switch {
	case env != "":
		if (fromFile != "" && fromFile != env) || (flagValue != "" && flagValue != env) {
			warnings = append(warnings, "MEERKAT_API_KEY env overrides the key given by --api-key-file/--api-key")
		}
		key = env
	case fromFile != "":
		key = fromFile
	default:
		key = flagValue
	}
	if key == "" {
		return "", warnings, fmt.Errorf("no API key configured — set --api-key-file or MEERKAT_API_KEY (or --api-key)")
	}
	return key, warnings, nil
}

// readAPIKeyFile reads a key from path, trimming surrounding whitespace.
// A file readable by other users is accepted but warned about.
func readAPIKeyFile(path string) (string, []string, error) {
	var warnings []string
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, fmt.Errorf("--api-key-file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("--api-key-file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf("%s is readable by other users (mode %04o); run chmod 600 on it",
			path, info.Mode().Perm()))
	}
	body, err := os.ReadFile(path) //nolint:gosec // G304: path is an operator-supplied flag value (--api-key-file), not attacker-influenced input.
	if err != nil {
		return "", warnings, fmt.Errorf("--api-key-file: %w", err)
	}
	key := strings.TrimSpace(string(body))
	if key == "" {
		return "", warnings, fmt.Errorf("--api-key-file %s is empty", path)
	}
	return key, warnings, nil
}
