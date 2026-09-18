package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

func withLintRegistry(t *testing.T, pages []kb.Page) {
	t.Helper()
	reg, err := collections.New(collections.FromPages("hub", pages))
	if err != nil {
		t.Fatal(err)
	}
	orig := activeRegistry
	activeRegistry = reg
	t.Cleanup(func() { activeRegistry = orig })
}

func TestLintCmd_FailsOnDanglingAndPrintsThem(t *testing.T) {
	withLintRegistry(t, []kb.Page{
		{ID: "a", Title: "A", Front: kb.Frontmatter{Related: []string{"b", "missing"}}},
		{ID: "b", Title: "B"},
		{ID: "p", Title: "P", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:nope", Hint: "x"}},
	})
	cmd := newLintCmd()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "2 dangling") {
		t.Fatalf("err = %v, want 2 dangling", err)
	}
	if !strings.Contains(out.String(), `hub:a: related "missing"`) || !strings.Contains(out.String(), `hub:p: pointer target "collection:nope"`) {
		t.Errorf("output = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "3 pages, 3 links, 2 dangling") {
		t.Errorf("summary = %q", errOut.String())
	}

	cmd = newLintCmd()
	out.Reset()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--json"})
	_ = cmd.Execute()
	var report collections.LinkReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if report.Pages != 3 || len(report.Dangling) != 2 {
		t.Errorf("report = %+v", report)
	}
}

func TestLintCmd_CleanContentPasses(t *testing.T) {
	withLintRegistry(t, []kb.Page{
		{ID: "a", Title: "A", Front: kb.Frontmatter{Related: []string{"b", "mcp://datadog"}}},
		{ID: "b", Title: "B"},
	})
	cmd := newLintCmd()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("clean content: %v", err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "0 dangling") {
		t.Errorf("out=%q err=%q", out.String(), errOut.String())
	}
}
