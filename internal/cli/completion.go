package cli

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

// completion.go centralises the dynamic value providers used by
// cobra's `RegisterFlagCompletionFunc` / `ValidArgsFunction`. They
// run inside the user's shell at TAB time, so they must be cheap.
// The data comes from whatever content this invocation serves — the
// same resolution order a normal run uses (--kb-dir/MEERKAT_KB_DIR,
// then --content-source/MEERKAT_CONTENT_SOURCE, then content-source.yaml
// discovery, then the build-time embed) — so a TAB on a command line
// that points somewhere else completes from there, not from the embed.

// completionContent re-resolves the content to serve from the flags on
// the command being completed, when either of them carries a location.
//
// This second resolution is not redundant. Cobra's hidden `__complete`
// command sets DisableFlagParsing, so when the root command's
// PersistentPreRunE runs, the whole command line is still `__complete`'s
// positional args and the --kb-dir/--content-source variables it reads
// are empty — content resolves from the environment only (#77). Cobra
// parses the real command's flags later, inside getCompletions, right
// before calling ValidArgsFunction: by then cmd.Flags() holds the values
// the user actually typed, which is exactly here.
//
// It re-runs the SAME resolution the hook runs (resolveContent), so
// precedence is identical: flag over environment, --kb-dir over
// --content-source. When neither flag is set there is nothing new to
// learn and it does no work at all, which keeps the common TAB — and
// the environment-variable path — as cheap as it was.
//
// Errors are returned, never printed: stdout is the completion protocol
// and a stray line on it corrupts the shell's parse. Every caller turns
// an error into "no completions".
//
// The re-resolution runs under completionResolveTimeout. For a local
// source it is a directory walk and finishes long before that; for a
// remote one (type: url, gcs, s3) it is a real network round trip —
// a conditional fetch or a metadata call — and an unreachable or slow
// store would otherwise hold the user's shell for as long as the SDK's
// own timeout, if it has one. Past the deadline the TAB simply offers
// nothing, like any other resolution error.
func completionContent(cmd *cobra.Command) error {
	if cmd == nil {
		// A subcommand driven directly, without a root command — the
		// package's own tests do this. Whatever the process globals
		// point at is the answer.
		return nil
	}
	kbDir, _ := cmd.Flags().GetString("kb-dir")
	contentSource, _ := cmd.Flags().GetString("content-source")
	if kbDir == "" && contentSource == "" {
		return nil
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, completionResolveTimeout)
	defer cancel()
	return resolveContent(ctx, kbDir, contentSource)
}

// completionResolveTimeout bounds completionContent's re-resolution. A
// second is well above a warm remote metadata call and still short
// enough that a TAB against a dead endpoint feels like "no
// completions", not a hung shell. A variable so a test can shorten it.
var completionResolveTimeout = time.Second

// completeSourceIDs lists every source.id from sources.yaml.
// Wired by `mk ingest --source <TAB>`.
func completeSourceIDs(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if err := completionContent(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	all, err := sources.All()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	out := make([]string, 0, len(all))
	for _, s := range all {
		if strings.HasPrefix(s.ID, toComplete) {
			out = append(out, s.ID)
		}
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completePageIDs returns every page id in the content this invocation
// serves. Wired by `mk show <TAB>` (positional) and `mk ingest --page`.
//
// Filter: if onlyStale is true, only pages whose status is
// placeholder / ingest-failed are returned (the set `mk ingest`
// would actually plan).
func completePageIDs(onlyStale bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if err := completionContent(cmd); err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		pages, err := kb.List()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		out := make([]string, 0, len(pages))
		for _, p := range pages {
			if onlyStale {
				if p.Front.Status != "placeholder" && p.Front.Status != "ingest-failed" {
					continue
				}
			}
			if toComplete == "" || strings.HasPrefix(p.ID, toComplete) {
				out = append(out, p.ID)
			}
		}
		// Sorted by kb.List already; trim is cheap.
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// completePrefixes returns the set of unique slash-segment prefixes
// across the served KB, useful for `--prefix`.
//
// E.g. for pages systems/backend/foo and systems/frontend/bar we
// emit "systems/", "systems/backend/", "systems/frontend/".
func completePrefixes(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if err := completionContent(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	pages, err := kb.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	seen := map[string]struct{}{}
	for _, p := range pages {
		// Build progressive prefixes: "a/b/c" -> "a/", "a/b/"
		parts := strings.Split(p.ID, "/")
		for i := 1; i < len(parts); i++ {
			pfx := strings.Join(parts[:i], "/") + "/"
			if toComplete == "" || strings.HasPrefix(pfx, toComplete) {
				seen[pfx] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}

// completeCategories returns the distinct frontmatter `category`
// values across the served KB.
func completeCategories(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return distinctFrontmatterValues(cmd, toComplete, func(p kb.Page) string {
		return p.Front.Category
	})
}

// completeOwners returns the distinct frontmatter `owner` values.
func completeOwners(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return distinctFrontmatterValues(cmd, toComplete, func(p kb.Page) string {
		return p.Front.Owner
	})
}

// completeTypes returns the distinct frontmatter `type` values across
// the served KB — OKF's required concept-kind field (SPEC.md §4.1).
// Wired by `mk list --type <TAB>`.
func completeTypes(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return distinctFrontmatterValues(cmd, toComplete, func(p kb.Page) string {
		return p.Front.Type
	})
}

// completeStatuses returns the documented status enum.
func completeStatuses(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	all := []string{"placeholder", "reviewed", "stale", "ingest-failed", "needs-research", "superseded"}
	out := all[:0:0]
	for _, s := range all {
		if strings.HasPrefix(s, toComplete) {
			out = append(out, s)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// distinctFrontmatterValues is the shared helper for category/owner.
func distinctFrontmatterValues(cmd *cobra.Command, prefix string, get func(kb.Page) string) ([]string, cobra.ShellCompDirective) {
	if err := completionContent(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	pages, err := kb.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	seen := map[string]struct{}{}
	for _, p := range pages {
		v := get(p)
		if v == "" {
			continue
		}
		if prefix == "" || strings.HasPrefix(v, prefix) {
			seen[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}
