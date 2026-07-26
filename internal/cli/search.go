package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/search"
)

func newSearchCmd() *cobra.Command {
	var (
		limit    int
		asJSON   bool
		showBody bool
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across the embedded wiki",
		Long: `Run a BM25 full-text search over every embedded wiki page.

Title and ID matches are boosted so page-name lookups (e.g. "FAM-Guide",
"PowerGrid") rank above incidental body mentions.

Examples:
  btkb search "crispy agent"
  btkb search "PGS PLC password"
  btkb search title:eviction        # field-targeted query
  btkb search "30 minute" --limit 20`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")

			idx, err := search.New()
			if err != nil {
				return fmt.Errorf("build index: %w", err)
			}
			defer idx.Close()

			results, err := idx.Query(query, limit)
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
					i+1, r.Page.ID, r.Score, r.Page.Title,
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
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum number of results")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output results as JSON")
	cmd.Flags().BoolVar(&showBody, "body", false, "Print the full body of every hit")
	return cmd
}

// oneLine collapses whitespace so snippets render on a single CLI line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
