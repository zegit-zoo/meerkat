package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// newLintCmd builds `mk lint`: the first librarian tool. It mounts the
// configured collections exactly as `mk search` would and fails on any
// `related:` entry or pointer target that does not resolve, so a content
// repository's CI can refuse to publish a dangling link.
func newLintCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Check related: links and pointer targets across the mounted collections",
		Long: `Resolve every page's related: entries and every type: pointer page's
target: against the mounted collections, and list the ones that do not
resolve.

A link is dangling when it names a page that does not exist
("systems/api", "flux:concepts/drift"), a collection that is not mounted
("collection:flux", "flux:…"), or does not parse at all. A pointer is
invalid when it has no target:, no hint:, or targets a page in its own
collection (use related: for that). External references (mcp://…,
ext:<scheme>:<target>, external:<name>) are never checked.

Exit status is 1 when anything dangles, so a content repository can run
this in CI. The same findings are reported, capped, as warnings in
/readyz and mk_list_collections; they never make a collection unready.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := registry()
			report := reg.LinkReport()
			if asJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
					return err
				}
			} else {
				for _, d := range report.Dangling {
					fmt.Fprintln(cmd.OutOrStdout(), d.String())
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "%d pages, %d links, %d dangling\n", report.Pages, report.Links, len(report.Dangling))
			}
			if !report.OK() {
				cmd.SilenceUsage = true
				return fmt.Errorf("%d dangling link(s)", len(report.Dangling))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the full report as JSON")
	return cmd
}
