package cli

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

func newListCmd() *cobra.Command {
	var (
		asJSON     bool
		prefix     string
		category   string
		status     string
		owner      string
		typ        string
		collection string
		listColls  bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List wiki pages, optionally filtered",
		Long: `List pages embedded in this meerkat binary.

Filters compose (AND):
  --prefix    page ID prefix, e.g. "systems/backend/"
  --category  frontmatter 'category' field, e.g. "policies"
  --status    frontmatter 'status' field, e.g. "placeholder", "reviewed"
  --owner     frontmatter 'owner' field, e.g. "team-payments"
  --type      frontmatter 'type' field, e.g. "BigQuery Table" (OKF's
              required concept-kind field, SPEC.md §4.1)

With several collections mounted, pages from all of them are listed in
configuration order and IDs print qualified ("<collection>:<page-id>");
--collection narrows the listing to one. --collections instead
enumerates the mounted collections themselves.

Default output is "id  title  status". --json adds frontmatter.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := registry()
			if listColls {
				return listCollections(cmd, reg, asJSON)
			}
			refs, err := reg.Pages(collection)
			if err != nil {
				return fmt.Errorf("list pages: %w", err)
			}

			keep := func(f kb.FilterFunc) {
				if f == nil {
					return
				}
				out := refs[:0:0]
				for _, r := range refs {
					if f(r.Page) {
						out = append(out, r)
					}
				}
				refs = out
			}
			keep(kb.ByPrefix(prefix))
			if category != "" {
				keep(kb.ByCategory(category))
			}
			if status != "" {
				keep(kb.ByStatus(status))
			}
			if owner != "" {
				keep(kb.ByOwner(owner))
			}
			if typ != "" {
				keep(kb.ByType(typ))
			}
			// Stable within a collection (by page ID) while preserving the
			// configured collection order across them — the same order
			// everything else routes in.
			sort.SliceStable(refs, func(i, j int) bool {
				if refs[i].Collection == refs[j].Collection {
					return refs[i].Page.ID < refs[j].Page.ID
				}
				return false
			})

			if asJSON {
				out := make([]map[string]any, len(refs))
				for i, r := range refs {
					p := r.Page
					entry := map[string]any{
						"id":         p.ID,
						"collection": r.Collection,
						"title":      p.Title,
						"category":   p.Front.Category,
						"status":     p.Front.Status,
						"owner":      p.Front.Owner,
						"type":       p.Front.Type,
						"source":     p.Front.Source,
					}
					if p.Front.FailureReason != "" {
						entry["failure_reason"] = p.Front.FailureReason
					}
					out[i] = entry
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
			}

			for _, r := range refs {
				st := r.Page.Front.Status
				if st == "" {
					st = "-"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-55s  %-12s  %s\n", displayID(reg, r), st, r.Page.Title)
				if r.Page.Front.FailureReason != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "%-55s  %-12s  ↳ %s\n", "", "", r.Page.Front.FailureReason)
				}
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\n%d pages\n", len(refs))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON (includes frontmatter)")
	cmd.Flags().BoolVar(&listColls, "collections", false,
		"List the mounted collections (name, type, provenance, page count) instead of pages")
	cmd.Flags().StringVar(&prefix, "prefix", "", "Only pages whose ID starts with this prefix")
	cmd.Flags().StringVar(&category, "category", "", "Only pages with this frontmatter category")
	cmd.Flags().StringVar(&status, "status", "", "Only pages with this frontmatter status")
	cmd.Flags().StringVar(&owner, "owner", "", "Only pages with this frontmatter owner")
	cmd.Flags().StringVar(&typ, "type", "", "Only pages with this frontmatter type (OKF's concept-kind field, e.g. \"BigQuery Table\")")
	_ = cmd.RegisterFlagCompletionFunc("prefix", completePrefixes)
	_ = cmd.RegisterFlagCompletionFunc("category", completeCategories)
	_ = cmd.RegisterFlagCompletionFunc("status", completeStatuses)
	_ = cmd.RegisterFlagCompletionFunc("owner", completeOwners)
	_ = cmd.RegisterFlagCompletionFunc("type", completeTypes)
	addCollectionFlag(cmd, &collection, "list")
	return cmd
}

// listCollections implements `mk list --collections`: what is mounted,
// where each collection's content came from, and how many pages it
// serves. It is the discovery surface for every other --collection /
// "<collection>:<page-id>" usage.
func listCollections(cmd *cobra.Command, reg *collections.Registry, asJSON bool) error {
	type entry struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Source     string `json:"source"`
		Pages      int    `json:"pages"`
		PageErrors string `json:"page_error,omitempty"`
	}
	out := make([]entry, 0, reg.Len())
	for _, c := range reg.All() {
		typ := c.Source.Type
		if typ == "" {
			// No content-source.yaml entry behind it: the embedded build,
			// or a --kb-dir directory (which carries no type of its own).
			typ = contentsource.TypeNone
		}
		e := entry{Name: c.Name, Type: typ, Source: c.Provenance}
		pages, err := c.Pages()
		if err != nil {
			// One unreadable collection must not hide the others: report
			// it in place rather than failing the whole listing.
			e.PageErrors = err.Error()
		} else {
			e.Pages = len(pages)
		}
		out = append(out, e)
	}
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}
	for _, e := range out {
		fmt.Fprintf(cmd.OutOrStdout(), "%-20s  %-8s  %5d pages  %s\n", e.Name, e.Type, e.Pages, e.Source)
		if e.PageErrors != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s  %s\n", "", "↳ "+e.PageErrors)
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "\n%d collections\n", len(out))
	return nil
}
