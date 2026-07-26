package cli

import (
	"runtime"
	"testing"
)

// TestCurrentVersion: at minimum every field is populated (defaults
// kick in when -ldflags isn't set).
func TestCurrentVersion(t *testing.T) {
	v := currentVersion()
	if v.Version == "" {
		t.Errorf("Version is empty (default should be 'dev')")
	}
	if v.GoVersion == "" || v.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", v.GoVersion, runtime.Version())
	}
	if v.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", v.OS, runtime.GOOS)
	}
	if v.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", v.Arch, runtime.GOARCH)
	}
}

// TestNewRootCmd: the cobra root has every documented group +
// command. If a future commit forgets to wire something up this
// catches it.
func TestNewRootCmd_HasAllCommands(t *testing.T) {
	root := NewRootCmd()

	want := []string{"search", "show", "list", "mcp", "http", "ingest", "update", "version"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing command: %q", w)
		}
	}

	// Subcommands of `mk ingest`.
	var ingest *string
	for _, c := range root.Commands() {
		if c.Name() == "ingest" {
			for _, sc := range c.Commands() {
				if sc.Name() == "sources" {
					n := sc.Name()
					ingest = &n
				}
			}
		}
	}
	if ingest == nil {
		t.Error("missing 'ingest sources' subcommand")
	}
}
