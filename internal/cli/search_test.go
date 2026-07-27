package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/search"
)

// TestSearchCmd_OversizedQueryIsRejected proves the CLI surface picks
// up the shared search-layer guard (internal/search/limits.go) for
// free, with no CLI-specific wiring: a pathological query comes back as
// a clean cobra error, not a panic or a multi-second hang.
func TestSearchCmd_OversizedQueryIsRejected(t *testing.T) {
	cmd := newSearchCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{strings.Repeat("x", 5000)})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for an oversized query")
	}
	if !strings.Contains(err.Error(), "invalid query") {
		t.Errorf("error = %q, want it to mention the invalid-query guard", err.Error())
	}
}

// TestSearchCmd_DefaultLimitMatchesSearchPackage guards against the
// CLI's --limit default drifting from the shared search.DefaultLimit
// constant that HTTP/MCP also rely on.
func TestSearchCmd_DefaultLimitMatchesSearchPackage(t *testing.T) {
	cmd := newSearchCmd()
	f := cmd.Flags().Lookup("limit")
	if f == nil {
		t.Fatal("no --limit flag registered")
	}
	want := fmt.Sprintf("%d", search.DefaultLimit)
	if f.DefValue != want {
		t.Errorf("--limit default = %q, want %q", f.DefValue, want)
	}
}
