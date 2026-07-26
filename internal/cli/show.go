package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

func newShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <page-id>",
		Short: "Print a single wiki page",
		Long: `Print the raw markdown for a single wiki page.

Page IDs are slash-separated paths from the wiki root, without the .md
suffix. Examples:
  btkb show index
  btkb show concepts/PowerGrid
  btkb show systems/BAF/access`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePageIDs(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			page, err := kb.Load(args[0])
			if err != nil {
				if errors.Is(err, kb.ErrNotFound) {
					return fmt.Errorf("page %q not found - try `btkb list`", args[0])
				}
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(page)
			}
			fmt.Fprintln(cmd.OutOrStdout(), page.Body)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON (page metadata + body)")
	return cmd
}
