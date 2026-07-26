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
// "embedded" or "disk:<path>". Set once per invocation by the root
// command's PersistentPreRunE (see kbDirFlag below), before any
// subcommand's RunE runs. Defaults to embedded so direct callers of
// newVersionCmd() in tests (which bypass the root command) still get
// a sane, correct answer: no --kb-dir means embedded.
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

	root := &cobra.Command{
		Use:   "meerkat",
		Short: "Meerkat — the vigilant guard and informer (knowledge-base CLI)",
		Long: `Meerkat embeds a knowledge-base wiki and exposes it via
CLI subcommands, an MCP server (for agent harnesses / OpenCode), and an
HTTP/OpenAPI server (for OpenWebUI).

All wiki content is bundled into the binary at build time and served
from there by default. No network access is required for search,
show, or list. Point --kb-dir (or MEERKAT_KB_DIR) at a content-repo
directory (the layout content-source.yaml describes and 'mk ingest'
writes into) to serve updated content without rebuilding the binary —
the embed remains the fallback when neither is set.

Page IDs are slash-paths from the wiki root without ".md" — e.g.
"concepts/Some-Concept", "systems/backend/some-service".

Short alias: 'mk' (installed as a symlink alongside meerkat).`,
		Example: `  # Knowledge base (offline)
  meerkat search "some term"
  meerkat show concepts/Some-Concept
  meerkat list --prefix systems/backend/
  meerkat list --category policies --status placeholder

  # Serve content from disk instead of the embedded build
  meerkat --kb-dir ./meerkat-kb search "some term"
  MEERKAT_KB_DIR=./meerkat-kb meerkat list`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// PersistentPreRunE resolves --kb-dir/MEERKAT_KB_DIR once per
		// invocation, before any subcommand's RunE, and points
		// internal/kb + internal/sources at the result (disk directory
		// or embedded fallback). It runs for every subcommand since none
		// of them define their own PersistentPreRun.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			dir := kbdir.Resolve(kbDirFlag)
			source, err := kbdir.Configure(dir)
			if err != nil {
				return err
			}
			kbSourceProvenance = source
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
			"degrades to empty rather than erroring.")

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
