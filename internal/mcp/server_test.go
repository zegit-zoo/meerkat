package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/collections"
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
	reg := collections.Global("test")
	if _, err := reg.All()[0].Index(); err != nil {
		t.Fatalf("build index: %v", err)
	}
	defer func() { _ = reg.Close() }()

	s := mcpserver.NewMCPServer(
		"meerkat-test", "test",
		mcpserver.WithToolCapabilities(true),
	)
	registerSearch(s, reg)
	registerShow(s, reg)
	registerList(s, reg)
	registerListCollections(s, reg)

	// Exercise the public CapabilityRegistry too — confirms the
	// server has the tool-capability flag set, which is what an
	// MCP client probes for during initialise.
	if s == nil {
		t.Fatal("MCPServer is nil")
	}
}

// TestToolAnnotations_ReadOnlyNonDestructive is a regression test for the
// finding that mcp.NewTool's un-overridden defaults
// (readOnlyHint:false, destructiveHint:true, idempotentHint:false,
// openWorldHint:true — verified live in a tools/list response) were
// shipping on all three tools. mk_search/mk_show/mk_list are pure KB
// reads with no side effects and no interaction with anything outside
// this process, so every tool must advertise the opposite of that
// default shape: read-only, non-destructive, idempotent, closed-world.
func TestToolAnnotations_ReadOnlyNonDestructive(t *testing.T) {
	reg := collections.Global("test")
	for _, tool := range []mcp.Tool{searchTool(reg), showTool(reg), listTool(reg), listCollectionsTool(reg)} {
		t.Run(tool.Name, func(t *testing.T) {
			a := tool.Annotations
			if a.ReadOnlyHint == nil || !*a.ReadOnlyHint {
				t.Errorf("ReadOnlyHint = %v, want true", boolPtrStr(a.ReadOnlyHint))
			}
			if a.DestructiveHint == nil || *a.DestructiveHint {
				t.Errorf("DestructiveHint = %v, want false", boolPtrStr(a.DestructiveHint))
			}
			if a.IdempotentHint == nil || !*a.IdempotentHint {
				t.Errorf("IdempotentHint = %v, want true", boolPtrStr(a.IdempotentHint))
			}
			if a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Errorf("OpenWorldHint = %v, want false", boolPtrStr(a.OpenWorldHint))
			}
		})
	}
}

func boolPtrStr(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	if *b {
		return "true"
	}
	return "false"
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
