package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	mhttp "github.com/zegit-zoo/meerkat/internal/http"
)

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
		host   string
		port   int
		apiKey string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the meerkat KB tools over HTTP/JSON with bearer auth",
		Long: `Run an HTTP/OpenAPI server. Endpoints:

  POST /search        full-text search
  POST /show          retrieve one page (body + frontmatter)
  POST /list          enumerate pages with filters
  GET  /openapi.json  schema (no auth)
  GET  /healthz       liveness (no auth)

Authentication: all data endpoints require an Authorization: Bearer
header carrying the configured API key. The key is supplied via
--api-key or the MEERKAT_API_KEY env var (env wins if both set).
The server refuses to start without a key — there is no anonymous
mode.

Register http://<host>:<port>/openapi.json with OpenWebUI as a Tool
Server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// MEERKAT_API_KEY env wins over --api-key flag.
			if v := os.Getenv("MEERKAT_API_KEY"); v != "" {
				if apiKey != "" && apiKey != v {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"warning: MEERKAT_API_KEY env overrides --api-key flag")
				}
				apiKey = v
			}
			if apiKey == "" {
				return fmt.Errorf("no API key configured — set --api-key or MEERKAT_API_KEY")
			}

			cfg := mhttp.Config{
				Addr:    mhttp.ResolveListenAddr(host, port),
				APIKey:  apiKey,
				Version: version,
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
	cmd.Flags().StringVar(&apiKey, "api-key", "",
		"Static bearer token. Required (or set MEERKAT_API_KEY).")
	return cmd
}
