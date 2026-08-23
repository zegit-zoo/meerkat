package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

func newShowCmd() *cobra.Command {
	var (
		asJSON     bool
		collection string
	)
	cmd := &cobra.Command{
		Use:   "show <page-id>",
		Short: "Print a single wiki page",
		Long: `Print the raw markdown for a single wiki page.

Page IDs are slash-separated paths from the wiki root, without the .md
suffix. Examples:
  mk show index
  mk show concepts/rate-limiting
  mk show systems/backend/access

With several collections mounted, every collection is tried in
configuration order. A page ID may be qualified as
"<collection>:<page-id>", or narrowed with --collection; a bare ID that
exists in more than one collection is an error listing the qualified
IDs to choose from, never a silent pick:
  mk show runbooks:incidents/paging
  mk show incidents/paging --collection runbooks

--json adds two OKF-derived advisory signals alongside the page's own
frontmatter (front): trust_tier (unverified | machine-confirmed |
human-reviewed, derived from front.verified — SPEC.md §5.3) and stale
(whether today is on/after front.stale_after — SPEC.md §5.5), plus the
collection the page was served from.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePageIDs(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := registry().Show(collection, args[0])
			if err != nil {
				if errors.Is(err, kb.ErrNotFound) {
					return fmt.Errorf("page %q not found - try `mk list`", args[0])
				}
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(newShowResult(ref))
			}
			fmt.Fprintln(cmd.OutOrStdout(), ref.Page.Body)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON (page metadata + body)")
	addCollectionFlag(cmd, &collection, "look in")
	return cmd
}

// showResult is the `mk show --json` wire shape: the page's stored
// fields (embedded, so id/path/title/body/front are promoted to the
// top level exactly as before this change) plus two OKF-derived
// advisory signals that are computed rather than stored — see
// kb.Frontmatter.TrustTier / IsStale — and so don't already appear on
// kb.Page's own JSON encoding, plus the collection the page came from.
//
// `id` is always the page's own, unqualified ID: the collection is
// reported as its own field rather than folded into the ID, so a
// consumer that round-trips an id (a link, a bookmark, `mk ingest`)
// is unaffected by how many collections happen to be mounted.
type showResult struct {
	kb.Page
	Collection string `json:"collection"`
	TrustTier  string `json:"trust_tier"`
	Stale      bool   `json:"stale"`
}

// newShowResult builds the mk show --json payload for a page reference.
func newShowResult(ref collections.PageRef) showResult {
	return showResult{
		Page:       ref.Page,
		Collection: ref.Collection,
		TrustTier:  ref.Page.Front.TrustTier(),
		Stale:      ref.Page.Front.IsStale(time.Now().UTC()),
	}
}
