package cli

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

func newListCmd() *cobra.Command {
	var (
		asJSON   bool
		prefix   string
		category string
		status   string
		owner    string
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

Default output is "id  title  status". --json adds frontmatter.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pages, err := kb.List()
			if err != nil {
				return fmt.Errorf("list pages: %w", err)
			}

			if f := kb.ByPrefix(prefix); f != nil {
				pages = kb.Filter(pages, f)
			}
			if category != "" {
				pages = kb.Filter(pages, kb.ByCategory(category))
			}
			if status != "" {
				pages = kb.Filter(pages, kb.ByStatus(status))
			}
			if owner != "" {
				pages = kb.Filter(pages, kb.ByOwner(owner))
			}
			sort.Slice(pages, func(i, j int) bool { return pages[i].ID < pages[j].ID })

			if asJSON {
				out := make([]map[string]any, len(pages))
				for i, p := range pages {
					entry := map[string]any{
						"id":       p.ID,
						"title":    p.Title,
						"category": p.Front.Category,
						"status":   p.Front.Status,
						"owner":    p.Front.Owner,
						"source":   p.Front.Source,
					}
					if p.Front.FailureReason != "" {
						entry["failure_reason"] = p.Front.FailureReason
					}
					out[i] = entry
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
			}

			for _, p := range pages {
				st := p.Front.Status
				if st == "" {
					st = "-"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-55s  %-12s  %s\n", p.ID, st, p.Title)
				if p.Front.FailureReason != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "%-55s  %-12s  ↳ %s\n", "", "", p.Front.FailureReason)
				}
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\n%d pages\n", len(pages))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON (includes frontmatter)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "Only pages whose ID starts with this prefix")
	cmd.Flags().StringVar(&category, "category", "", "Only pages with this frontmatter category")
	cmd.Flags().StringVar(&status, "status", "", "Only pages with this frontmatter status")
	cmd.Flags().StringVar(&owner, "owner", "", "Only pages with this frontmatter owner")
	_ = cmd.RegisterFlagCompletionFunc("prefix", completePrefixes)
	_ = cmd.RegisterFlagCompletionFunc("category", completeCategories)
	_ = cmd.RegisterFlagCompletionFunc("status", completeStatuses)
	_ = cmd.RegisterFlagCompletionFunc("owner", completeOwners)
	return cmd
}
