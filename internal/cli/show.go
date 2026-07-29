package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
  mk show index
  mk show concepts/rate-limiting
  mk show systems/backend/access

--json adds two OKF-derived advisory signals alongside the page's own
frontmatter (front): trust_tier (unverified | machine-confirmed |
human-reviewed, derived from front.verified — SPEC.md §5.3) and stale
(whether today is on/after front.stale_after — SPEC.md §5.5).`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePageIDs(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			page, err := kb.Load(args[0])
			if err != nil {
				if errors.Is(err, kb.ErrNotFound) {
					return fmt.Errorf("page %q not found - try `mk list`", args[0])
				}
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(newShowResult(page))
			}
			fmt.Fprintln(cmd.OutOrStdout(), page.Body)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON (page metadata + body)")
	return cmd
}

// showResult is the `mk show --json` wire shape: the page's stored
// fields (embedded, so id/path/title/body/front are promoted to the
// top level exactly as before this change) plus two OKF-derived
// advisory signals that are computed rather than stored — see
// kb.Frontmatter.TrustTier / IsStale — and so don't already appear on
// kb.Page's own JSON encoding.
type showResult struct {
	kb.Page
	TrustTier string `json:"trust_tier"`
	Stale     bool   `json:"stale"`
}

// newShowResult builds the mk show --json payload for page.
func newShowResult(page kb.Page) showResult {
	return showResult{
		Page:      page,
		TrustTier: page.Front.TrustTier(),
		Stale:     page.Front.IsStale(time.Now().UTC()),
	}
}
