package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// versionInfo is the structured version payload printed by `btkb version`
// and consumed by `btkb diagnose` for reporting.
type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	KBCommit  string `json:"kb_commit"`
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
				"meerkat %s (%s) built %s\n  knowledge base: %s\n  runtime: %s on %s/%s\n",
				info.Version, info.Commit, info.Date,
				info.KBCommit, info.GoVersion, info.OS, info.Arch,
			)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}
