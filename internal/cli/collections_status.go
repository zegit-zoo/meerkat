package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/collections"
)

// collections_status.go is `mk collections status` (meerkat-mob#25 part
// C, MK-FRESH-08): freshness assertable from a script.
//
// What a short-lived process can honestly say: it built its own index a
// moment ago, so its on-disk half is always current. What it adds is the
// remote half. For every collection with `remote_check`, it asks the
// upstream once, now, and NEVER pulls, whatever `on_divergence` says.
// The CLI reports, and the long-running server acts. A running server's
// own view is mk_list_collections.

func newCollectionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collections",
		Short: "Inspect the mounted collections",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newCollectionsStatusCmd())
	return cmd
}

// statusRow is one collection in `mk collections status`.
type statusRow struct {
	Collection string                `json:"collection"`
	Freshness  collections.Freshness `json:"freshness"`
	// RemoteProblem is "config" or "network" when a remote check could
	// not answer, and "" otherwise.
	RemoteProblem string `json:"remote_problem,omitempty"`
}

func newCollectionsStatusCmd() *cobra.Command {
	var asJSON, check bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report how current each type: local collection with a refresh: block is",
		Long: `Report the freshness of every type: local collection that has a
"refresh:" block in content-source.yaml. For each one that also sets
"remote_check", the upstream is asked once, now, with the same hardened
git ls-remote the server uses. Nothing is ever pulled, whatever
"on_divergence" says: this command reports, the server acts.

States here are current, behind-remote and unknown (see
docs/design/hot-reload.md). In a freshly started process the on-disk
half is always current. The index was just built. dirty and diverged
come only from a running server's pull, so this command never reports
them: a dirty or diverged checkout that is behind reads behind-remote.

With --check, the exit status is 1 when any collection is behind-remote,
or when a configured remote check could not run because of the
checkout's own configuration (not a git working tree, a detached HEAD,
no upstream, a refused name). It is 0 for current and unknown,
including an unreachable remote: a CI job must not fail because a
remote was briefly down. The output says which problem it was.

--check does not catch a checkout that fetched the remote's tip without
merging it. The check reads git's files and never walks history
(MK-SEC-12), so that checkout reads unknown and passes. Where CI
checkouts fetch, compare explicitly as well:
git rev-list --count HEAD..@{upstream} prints 0 only when nothing is
left to merge.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rows := collectionStatus(cmd)
			out := cmd.OutOrStdout()
			if asJSON {
				if err := json.NewEncoder(out).Encode(rows); err != nil {
					return err
				}
			} else {
				printStatus(out, rows)
			}
			if !check {
				return nil
			}
			var failed []string
			for _, r := range rows {
				// dirty and diverged never occur here (only a server's pull
				// decides them); they stay so the rule reads like the
				// server's advisory set.
				switch {
				case r.Freshness.State == collections.FreshBehindRemote,
					r.Freshness.State == collections.FreshDirty,
					r.Freshness.State == collections.FreshDiverged:
					failed = append(failed, fmt.Sprintf("%s is %s", r.Collection, r.Freshness.State))
				case r.RemoteProblem == "config":
					failed = append(failed, fmt.Sprintf("%s: remote check could not run (%s)", r.Collection, r.Freshness.Note))
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("freshness check failed: %s", strings.Join(failed, "; "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&check, "check", false,
		"Exit 1 when any collection is behind-remote, or its remote check could not run because of its configuration")
	return cmd
}

// collectionStatus runs one remote check per collection that asks for
// one, and returns every freshness record, in configuration order.
func collectionStatus(cmd *cobra.Command) []statusRow {
	rows := []statusRow{}
	for _, c := range registry().All() {
		if _, ok := c.Freshness(); !ok {
			continue
		}
		if r := c.Source.Refresh; r != nil && r.RemoteCheck > 0 {
			// A verdict, never a failure: whatever stops the check is on
			// the record, and the log detail is not needed here.
			_, _ = c.ProbeRemote(cmd.Context())
		}
		f, _ := c.Freshness()
		rows = append(rows, statusRow{Collection: c.Name, Freshness: f, RemoteProblem: f.RemoteProblem()})
	}
	return rows
}

func printStatus(w interface{ Write([]byte) (int, error) }, rows []statusRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no type: local collection has a refresh: block; nothing to report")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COLLECTION\tSTATE\tCOMMIT\tREMOTE\tNOTE")
	for _, r := range rows {
		f := r.Freshness
		note := f.Note
		if r.RemoteProblem != "" && note != "" {
			note = r.RemoteProblem + " problem: " + note
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Collection, f.State, short(f.OnDiskCommit), short(f.Remote), note)
	}
	_ = tw.Flush()
}

// short abbreviates a commit to twelve characters; anything else is
// printed as is.
func short(s string) string {
	if len(s) >= 40 {
		return s[:12]
	}
	return s
}
