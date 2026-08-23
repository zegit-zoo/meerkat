package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/search"
)

func newSearchCmd() *cobra.Command {
	var (
		limit      int
		asJSON     bool
		showBody   bool
		collection string
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across the embedded wiki",
		Long: `Run a BM25 full-text search over every embedded wiki page.

Title and ID matches are boosted so page-name lookups (e.g. "onboarding",
"rate-limiting") rank above incidental body mentions.

With several collections mounted, every collection is searched and the
hits are merged by score; --collection narrows it to one. Result IDs are
printed qualified ("<collection>:<page-id>") whenever more than one
collection is mounted, so they can be pasted straight into 'mk show'.

Examples:
  mk search "rate limiting"
  mk search "retention policy"
  mk search title:eviction        # field-targeted query
  mk search "30 minute" --limit 20
  mk search "incident" --collection runbooks`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")

			reg := registry()
			defer func() { _ = reg.Close() }()

			results, err := reg.Search(cmd.Context(), collection, query, limit)
			if err != nil {
				return err
			}

			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(results)
			}

			if len(results) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "no results for %q\n", query)
				return nil
			}

			for i, r := range results {
				fmt.Fprintf(cmd.OutOrStdout(),
					"%d. %s  (score %.2f)\n   %s\n",
					i+1, displayID(reg, collections.PageRef{Collection: r.Collection, Page: r.Page}), r.Score, r.Page.Title,
				)
				if r.Snippet != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "   %s\n", oneLine(r.Snippet))
				}
				if showBody {
					fmt.Fprintln(cmd.OutOrStdout(), strings.Repeat("-", 60))
					fmt.Fprintln(cmd.OutOrStdout(), r.Page.Body)
					fmt.Fprintln(cmd.OutOrStdout(), strings.Repeat("-", 60))
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", search.DefaultLimit, "Maximum number of results")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output results as JSON")
	cmd.Flags().BoolVar(&showBody, "body", false, "Print the full body of every hit")
	addCollectionFlag(cmd, &collection, "search")
	return cmd
}

// oneLine collapses whitespace so snippets render on a single CLI line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
