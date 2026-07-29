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
	KBSource  string `json:"kb_source"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func currentVersion() versionInfo {
	return versionInfo{
		Version:   version,
		Commit:    commit,
		Date:      date,
		KBCommit:  kbCommit,
		KBSource:  kbSourceProvenance,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
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
				"meerkat %s (%s) built %s\n  knowledge base: %s (source: %s)\n  runtime: %s on %s/%s\n",
				info.Version, info.Commit, info.Date,
				info.KBCommit, info.KBSource, info.GoVersion, info.OS, info.Arch,
			)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}
