package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// A deposit's target is the deepest attempted collection the depositor
// can see; collections outside their view, and names that are not one
// key segment, are dropped from what is written (meerkat-mob#38).
func TestReportOutcome_TargetIsLimitedToTheCallersView(t *testing.T) {
	reg, err := collections.New(
		collections.FromPages("mine", []kb.Page{{ID: "a", Title: "A"}}),
		collections.FromPages("victim", []kb.Page{{ID: "b", Title: "B"}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	g := authz.NewGrants(authz.Identity{Subject: "tenant", Issuer: "https://issuer.example"},
		map[string][]authz.Capability{"mine": {authz.CapRead, authz.CapIntakeWrite}})
	ctx := authz.NewContext(context.Background(), g)

	cases := []struct {
		attempted     []any
		wantAttempted []string
		wantTarget    string
	}{
		{[]any{"mine", "victim"}, []string{"mine"}, "mine"},
		{[]any{"victim"}, nil, "unrouted"},
		{[]any{"..", "mine/../victim", "./victim"}, nil, "unrouted"},
		{[]any{"victim", "mine"}, []string{"mine"}, "mine"},
	}
	for i, c := range cases {
		dir := filepath.Join(t.TempDir(), "intake")
		store, err := memory.OpenLocal(dir)
		if err != nil {
			t.Fatal(err)
		}
		f := outcomeFixture{reg: reg, opts: transportOptions{Outcome: OutcomeOptions{Intake: store}}, intake: dir}
		out := callOutcome(t, ctx, f, map[string]any{
			"outcome": "gave_up", "attempted": c.attempted,
			"fallback": map[string]any{"kind": "web", "summary": "s", "sources": []any{"https://example.com/"}},
		})
		if out["intake"] != "written" {
			t.Fatalf("case %d: %v", i, out)
		}
		pages := intakeFiles(t, dir)
		if len(pages) != 1 {
			t.Fatalf("case %d: %d intake objects", i, len(pages))
		}
		front := pages[0][strings.Index(pages[0], "{"):strings.Index(pages[0], "\n---\n#")]
		var fm struct {
			Attempted []string `json:"attempted"`
			TargetKB  string   `json:"target_kb"`
		}
		if err := json.Unmarshal([]byte(front), &fm); err != nil {
			t.Fatalf("case %d: frontmatter %q: %v", i, front, err)
		}
		if strings.Join(fm.Attempted, ",") != strings.Join(c.wantAttempted, ",") || fm.TargetKB != c.wantTarget {
			t.Errorf("case %d: attempted %v target %q; want %v %q", i, fm.Attempted, fm.TargetKB, c.wantAttempted, c.wantTarget)
		}
	}
}
