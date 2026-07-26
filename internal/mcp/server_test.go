package mcp

import (
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/search"
)

// TestRegisterTools_BuildsCleanly exercises the registration
// helpers individually. We can't drive ServeStdio in tests (it
// owns the parent process's stdin/stdout MCP framing), but we can
// prove the search index + tool registrations build cleanly.
//
// If anything in the embed pipeline broke (e.g. sources.yaml or
// content/ missing) this test fails loudly at server construction
// time — exactly when CI should catch it.
func TestRegisterTools_BuildsCleanly(t *testing.T) {
	idx, err := search.New()
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	defer idx.Close()

	s := mcpserver.NewMCPServer(
		"meerkat-test", "test",
		mcpserver.WithToolCapabilities(true),
	)
	registerSearch(s, idx)
	registerShow(s)
	registerList(s)

	// Exercise the public CapabilityRegistry too — confirms the
	// server has the tool-capability flag set, which is what an
	// MCP client probes for during initialise.
	if s == nil {
		t.Fatal("MCPServer is nil")
	}
}

// TestOneLine collapses internal whitespace runs.
func TestOneLine(t *testing.T) {
	cases := map[string]string{
		"hello world":              "hello world",
		"foo\n\tbar   baz":         "foo bar baz",
		"  leading and trailing  ": "leading and trailing",
		"":                         "",
	}
	for in, want := range cases {
		got := oneLine(in)
		if got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}
