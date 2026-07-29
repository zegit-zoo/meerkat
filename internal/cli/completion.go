package cli

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

// completion.go centralises the dynamic value providers used by
// cobra's `RegisterFlagCompletionFunc` / `ValidArgsFunction`. They
// run inside the user's shell at TAB time, so they must be cheap.
// All data comes from the KB embedded in the binary at build time —
// no network, no disk reads outside the embed.

// completeSourceIDs lists every source.id from sources.yaml.
// Wired by `mk ingest --source <TAB>`.
func completeSourceIDs(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
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

// completePageIDs returns every embedded page id.
// Wired by `mk show <TAB>` (positional) and `mk ingest --page`.
//
// Filter: if onlyStale is true, only pages whose status is
// placeholder / ingest-failed are returned (the set `mk ingest`
// would actually plan).
func completePageIDs(onlyStale bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
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
// across the embedded KB, useful for `--prefix`.
//
// E.g. for pages systems/backend/foo and systems/frontend/bar we
// emit "systems/", "systems/backend/", "systems/frontend/".
func completePrefixes(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
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
// values across the embedded KB.
func completeCategories(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return distinctFrontmatterValues(toComplete, func(p kb.Page) string {
		return p.Front.Category
	})
}

// completeOwners returns the distinct frontmatter `owner` values.
func completeOwners(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return distinctFrontmatterValues(toComplete, func(p kb.Page) string {
		return p.Front.Owner
	})
}

// completeTypes returns the distinct frontmatter `type` values across
// the embedded KB — OKF's required concept-kind field (SPEC.md §4.1).
// Wired by `mk list --type <TAB>`.
func completeTypes(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return distinctFrontmatterValues(toComplete, func(p kb.Page) string {
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
func distinctFrontmatterValues(prefix string, get func(kb.Page) string) ([]string, cobra.ShellCompDirective) {
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
