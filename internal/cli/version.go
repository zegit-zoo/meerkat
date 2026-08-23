package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// versionInfo is the structured version payload printed by `mk version`.
type versionInfo struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	KBCommit string `json:"kb_commit"`
	// KBSource is the provenance of the content actually being served:
	// "embedded" (the build-time embed), "disk:<path>" (--kb-dir /
	// MEERKAT_KB_DIR, or a type: local content-source.yaml — an
	// arbitrary, unverified directory), or "url:<url>@<digest>" (a
	// type: url content-source.yaml). Distinct from KBCommit, which
	// always names the build-time embedded content's pinned commit
	// regardless of KBSource — a disk-served KB has no such pin, and
	// reporting one here would wrongly imply the cosign-signed release
	// covers content it never saw. A url: source sits in between: unlike
	// disk:, it IS content-verified (FetchURL checks the sha256 before
	// ever extracting or serving it) — but it's still not the build's own
	// pinned commit, so it gets its own prefix rather than being folded
	// into either of the other two.
	// KBSource stays a single string for a single-collection deployment
	// (i.e. every configuration that predates collections), reporting
	// exactly what it always did. A multi-collection deployment reports
	// "collections:<n>" here and the per-collection detail in
	// Collections below, rather than concatenating N provenance strings
	// into one field whose shape depends on the config.
	KBSource string `json:"kb_source"`
	// Collections is what this binary actually serves, one entry per
	// mounted collection, in configuration order.
	Collections []collectionInfo `json:"collections"`
	GoVersion   string           `json:"go_version"`
	OS          string           `json:"os"`
	Arch        string           `json:"arch"`
}

// collectionInfo is one mounted collection's entry in `mk version`.
// Source is that collection's own provenance, in the same vocabulary
// KBSource uses for the single-collection case ("embedded",
// "disk:<path>", "url:<url>@<digest>", "gcs://<bucket>/<object>@<gen>").
type collectionInfo struct {
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
	Source string `json:"source"`
}

func currentVersion() versionInfo {
	reg := registry()
	cols := make([]collectionInfo, 0, reg.Len())
	for _, c := range reg.All() {
		cols = append(cols, collectionInfo{Name: c.Name, Type: c.Source.Type, Source: c.Provenance})
	}
	return versionInfo{
		Version:     version,
		Commit:      commit,
		Date:        date,
		KBCommit:    kbCommit,
		KBSource:    kbSourceProvenance,
		Collections: cols,
		GoVersion:   runtime.Version(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
	}
}

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := currentVersion()
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"meerkat %s (%s) built %s\n  knowledge base: %s (source: %s)\n",
				info.Version, info.Commit, info.Date, info.KBCommit, info.KBSource,
			)
			// Only itemised when there's more than one: for a single
			// collection the line above already said everything, and
			// repeating it under a "collections:" heading would just be
			// noise for the deployment shape almost everyone runs.
			if len(info.Collections) > 1 {
				for _, c := range info.Collections {
					fmt.Fprintf(cmd.OutOrStdout(), "    %-20s %s\n", c.Name, c.Source)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  runtime: %s on %s/%s\n", info.GoVersion, info.OS, info.Arch)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}
