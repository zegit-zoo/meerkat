// Package cli wires together the cobra command tree for meerkat.
//
// Subcommand implementations live in sibling files (search.go,
// show.go, list.go, version.go). Future commands (mcp, http, update,
// ingest, diagnose) plug into the same groups as they're added.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
	"github.com/zegit-zoo/meerkat/internal/update"
)

// notifySkipCommands lists subcommands where the post-run "newer
// version available" nag would be noise. The user is already
// version-aware (version, update) or the surface is non-interactive
// (mcp serve, http serve, completion).
var notifySkipCommands = map[string]bool{
	"version":    true,
	"update":     true,
	"mcp":        true,
	"http":       true,
	"completion": true,
}

// Build-time variables wired via -ldflags. See Makefile and
// .goreleaser.yaml for the source of truth.
var (
	version  = "dev"
	commit   = "unknown"
	date     = "unknown"
	kbCommit = "unknown"
)

// kbSourceProvenance is the kb_source value `mk version` reports:
// "embedded", "disk:<path>" (--kb-dir/MEERKAT_KB_DIR, or a type: local
// content-source.yaml), or "url:<url>@<digest>" (a type: url
// content-source.yaml — see contentsource.URLProvenance). Set once per
// invocation by the root command's PersistentPreRunE, before any
// subcommand's RunE runs. Defaults to embedded so direct callers of
// newVersionCmd() in tests (which bypass the root command) still get
// a sane, correct answer: no content configured means embedded.
var kbSourceProvenance = kbdir.SourceEmbedded

// Cobra command groups so `meerkat --help` clusters subcommands by
// purpose instead of one alphabetised wall.
const (
	groupKB     = "kb"
	groupServer = "server"
	groupOps    = "ops"
)

// NewRootCmd builds the top-level meerkat command. Kept as a
// constructor so tests can spin up isolated trees.
func NewRootCmd() *cobra.Command {
	var kbDirFlag string
	var contentSourceFlag string

	root := &cobra.Command{
		Use:   "meerkat",
		Short: "Meerkat — the vigilant guard and informer (knowledge-base CLI)",
		Long: `Meerkat embeds a knowledge-base wiki and exposes it via
CLI subcommands, an MCP server (for agent harnesses / OpenCode), and an
HTTP/OpenAPI server (for OpenWebUI).

All wiki content is bundled into the binary at build time and served
from there by default. No network access is required for search,
show, or list. What gets served instead is resolved in priority order:

  1. --kb-dir (or MEERKAT_KB_DIR) — an explicit content-repo directory
     (the layout content-source.yaml describes and 'mk ingest' writes
     into). Wins outright over everything below.
  2. --content-source (or MEERKAT_CONTENT_SOURCE) — an explicit path to
     a content-source.yaml.
  3. <user config dir>/meerkat/content-source.yaml
  4. ./content-source.yaml in the working directory
  5. the embedded build (the fallback when none of the above apply)

content-source.yaml's "content.type" may be none, local, url or gcs at
runtime (git/submodule are build-time only — 'make sync' — since they
need git and a working tree; using one here fails with an error rather
than silently serving nothing). type: url fetches an HTTPS .tar.gz,
verifies it against a required sha256, and caches the extracted result
locally, keyed by that digest. type: gcs loads a Google Cloud Storage
.tar.gz object or bucket prefix, authenticating via Application Default
Credentials and caching by object generation — see
content-source.example.yaml.

A content-source.yaml may instead declare a "collections:" list of
named sources with heterogeneous backends, all mounted at once. Then:

  mk list --collections            enumerate what's mounted
  mk search "term"                 search across every collection
  mk search "term" --collection x  search just collection x
  mk show x:concepts/Thing         a page ID qualified by collection

Page IDs are slash-paths from the wiki root without ".md" — e.g.
"concepts/Some-Concept", "systems/backend/some-service" — optionally
prefixed with "<collection>:" when several collections are mounted.

Short alias: 'mk' (installed as a symlink alongside meerkat).`,
		Example: `  # Knowledge base (offline)
  meerkat search "some term"
  meerkat show concepts/Some-Concept
  meerkat list --prefix systems/backend/
  meerkat list --category policies --status placeholder

  # Serve content from disk instead of the embedded build
  meerkat --kb-dir ./meerkat-kb search "some term"
  MEERKAT_KB_DIR=./meerkat-kb meerkat list

  # Serve content resolved from a content-source.yaml (type: none|local|url|gcs)
  meerkat --content-source ./content-source.yaml list

  # Multiple named collections mounted at once
  meerkat list --collections
  meerkat search "incident" --collection runbooks
  meerkat show runbooks/index --collection runbooks`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// PersistentPreRunE resolves the content to serve once per
		// invocation, before any subcommand's RunE, and points
		// internal/kb + internal/sources at the result (disk directory
		// or embedded fallback). It runs for every subcommand since none
		// of them define their own PersistentPreRun.
		//
		// --kb-dir/MEERKAT_KB_DIR is resolved first and, if set, wins
		// outright — this branch is unchanged from before content-source
		// resolution existed. Only when it's unset does content-source.yaml
		// discovery (internal/contentsource.ResolveRuntime) run.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if dir := kbdir.Resolve(kbDirFlag); dir != "" {
				source, err := kbdir.Configure(dir)
				if err != nil {
					return err
				}
				kbSourceProvenance = source
				activeRegistry = collections.Global(source)
				// --kb-dir means "ignore content-source.yaml entirely", so
				// no auth: block is discovered either. `mk mcp serve-http
				// --kb-dir X` gets its policy from --auth-config or runs
				// unauthenticated — the same rule as for content.
				activeAuth = nil
				return nil
			}
			resolved, err := contentsource.ResolveRuntimeCollections(cmd.Context(), contentSourceFlag)
			if err != nil {
				return err
			}
			// The auth: block lives in the same content-source.yaml the
			// collections came from. Reading it here — once per
			// invocation, alongside content resolution — is what lets `mk
			// mcp serve-http` pick it up without re-running discovery.
			// It is inert for every other subcommand.
			auth, err := contentsource.LoadRuntimeAuth(contentSourceFlag)
			if err != nil {
				return err
			}
			activeAuth = auth
			// Point the process-global KB filesystem at the FIRST resolved
			// collection. For every single-collection configuration (which
			// is every configuration that predates collections) that is
			// simply "the" collection, and this line is exactly the call
			// this function always made. For a multi-collection config it
			// is what internal/sources, `mk ingest` and shell completion —
			// none of which are collection-aware yet — read through; the
			// collection-aware surfaces (search/show/list, MCP, HTTP) go
			// through the registry below instead and see all of them.
			primary := resolved[0]
			if _, err := kbdir.ConfigureLayout(primary.Dir, primary.Source.Layout); err != nil {
				return err
			}
			reg, err := collections.Open(resolved)
			if err != nil {
				return err
			}
			activeRegistry = reg
			kbSourceProvenance = reg.Provenance()
			return nil
		},
		// PersistentPostRun fires after every (sub-)command's RunE.
		// We use it to nag about new releases — but only when the
		// command was interactive (stderr is a TTY) and isn't on the
		// skip list. Cache + 24h freshness lives in internal/update,
		// so this never blocks the current invocation on the network.
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if notifySkipCommands[topLevelName(cmd)] {
				return
			}
			if !term.IsTerminal(int(os.Stderr.Fd())) {
				return
			}
			update.MaybeNotify(cmd.Context(), version, os.Stderr)
		},
	}

	root.PersistentFlags().StringVar(&kbDirFlag, "kb-dir", "",
		"Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, "+
			"ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. "+
			"The directory must exist; a missing wiki/ingestion/templates subdirectory inside it "+
			"degrades to empty rather than erroring. Wins over --content-source / content-source.yaml "+
			"discovery below.")
	root.PersistentFlags().StringVar(&contentSourceFlag, "content-source", "",
		"Path to a content-source.yaml describing where to serve KB content from "+
			"(content.type: none|local|url|gcs, or a collections: list of named sources — "+
			"git/submodule are build-time-only, 'make sync'). "+
			"Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this "+
			"nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, "+
			"then ./content-source.yaml, then the embedded build.")

	root.AddGroup(
		&cobra.Group{ID: groupKB, Title: "Knowledge base (always available, offline):"},
		&cobra.Group{ID: groupServer, Title: "Servers:"},
		&cobra.Group{ID: groupOps, Title: "Operations:"},
	)

	addToGroup := func(g string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = g
			root.AddCommand(c)
		}
	}
	addToGroup(groupKB, newSearchCmd(), newShowCmd(), newListCmd())
	addToGroup(groupServer, newMCPCmd(), newHTTPCmd())
	addToGroup(groupOps, newIngestCmd(), newUpdateCmd(), newVersionCmd())
	return root
}

// Execute runs the root command and returns an exit code suitable
// for os.Exit. Errors are printed to stderr; usage lines are
// suppressed so the user only sees the message.
func Execute() int {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "meerkat: %v\n", err)
		return 1
	}
	return 0
}

// topLevelName walks up the cobra parent chain to find the
// subcommand directly under root (e.g. "ingest" for "mk ingest
// sources"). Used to gate the update nag.
func topLevelName(cmd *cobra.Command) string {
	for cmd != nil && cmd.HasParent() {
		if !cmd.Parent().HasParent() {
			return cmd.Name()
		}
		cmd = cmd.Parent()
	}
	return ""
}
