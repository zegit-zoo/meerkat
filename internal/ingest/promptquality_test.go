package ingest

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// promptFixture: root -> {datadog, flux}. The datadog pointer's hint
// says "Dashboards.", nothing about alerts or API keys.
func promptFixture(t *testing.T) (*collections.Registry, *traversal.Log) {
	t.Helper()
	ctx := context.Background()
	root := collections.FromPages("root", []kb.Page{
		{ID: "datadog", Title: "Datadog", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:datadog", Hint: "Dashboards."}},
		{ID: "flux", Title: "Flux", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux", Hint: "Flux CD: sources, kustomizations, drift."}},
	})
	root.Tree = &contentsource.TreeNode{Name: "root", Path: "root", Depth: 0}
	datadog := collections.FromPages("datadog", []kb.Page{{ID: "monitors", Title: "Monitors"}})
	datadog.Tree = &contentsource.TreeNode{Name: "datadog", Path: "root/datadog", Depth: 1, Parent: "root"}
	flux := collections.FromPages("flux", []kb.Page{{ID: "concepts/drift", Title: "Drift"}})
	flux.Tree = &contentsource.TreeNode{Name: "flux", Path: "root/flux", Depth: 1, Parent: "root"}
	reg, err := collections.New(root, datadog, flux)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	log, err := traversal.Open(ctx, &traversal.Config{Backend: "local", Path: t.TempDir(), HMACKeyEnv: "MEERKAT_TEST_PATH_KEY"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(e traversal.Entry) {
		t.Helper()
		if _, err := log.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// hint: three gave up in datadog asking about alerts / api keys.
	rec(traversal.Entry{Session: "a", Outcome: "gave_up", InitialQuery: "datadgo alert routing", Attempted: []string{"root", "datadog"}})
	rec(traversal.Entry{Session: "b", Outcome: "gave_up", InitialQuery: "datadgo alert routing", Attempted: []string{"root", "datadog"}})
	rec(traversal.Entry{Session: "c", Outcome: "not_found", InitialQuery: "rotate the datadog api key", Attempted: []string{"root", "datadog"}})
	// Not a hint problem: the flux hint mentions "drift".
	rec(traversal.Entry{Session: "d", Outcome: "gave_up", InitialQuery: "drift remediation", Attempted: []string{"root", "flux"}})
	rec(traversal.Entry{Session: "e", Outcome: "gave_up", InitialQuery: "drift remediation", Attempted: []string{"root", "flux"}})
	// description: found only via the fuzzy stage, twice.
	rec(traversal.Entry{Session: "f", Outcome: "found", InitialQuery: "kustomizaton reconcile", Attempted: []string{"root", "flux"}, Pages: []string{"flux:concepts/drift"}, Stages: map[string]int{"exact": 1, "fuzzy": 1}})
	rec(traversal.Entry{Session: "g", Outcome: "found", InitialQuery: "helmrelase drift", Attempted: []string{"flux"}, Stages: map[string]int{"prefix": 1}})
	// tool: gave up without leaving the root.
	rec(traversal.Entry{Session: "h", Outcome: "gave_up", InitialQuery: "who is on call", Attempted: []string{"root"}})
	rec(traversal.Entry{Session: "i", Outcome: "gave_up", InitialQuery: "who is on call"})
	// route: started in the root and took a wrong turn (session f is
	// one too: WrongTurns 0 there, so only these two count).
	rec(traversal.Entry{Session: "j", Outcome: "found", InitialQuery: "flux drift", Attempted: []string{"root", "datadog", "flux"}, WrongTurns: 1})
	rec(traversal.Entry{Session: "k", Outcome: "found", InitialQuery: "flux drift", Attempted: []string{"root", "datadog", "flux"}, WrongTurns: 1})
	// Below the bar: one session alone reports nothing.
	rec(traversal.Entry{Session: "l", Outcome: "gave_up", InitialQuery: "grafana panels", Attempted: []string{"root", "flux"}})
	// No query: ignored entirely.
	rec(traversal.Entry{Session: "m", Outcome: "gave_up", Attempted: []string{"root", "datadog"}})
	return reg, log
}

func TestLibrarian_PromptQualityFindsRewriteTargets(t *testing.T) {
	ctx := context.Background()
	reg, log := promptFixture(t)
	rep, err := Librarian(ctx, reg, nil, LibrarianOpts{Log: log, Days: 2})
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]PromptFinding{}
	for _, f := range rep.PromptQuality {
		byTarget[f.Target] = f
	}
	if len(rep.PromptQuality) != 4 || rep.Count(FindingPromptQuality) != 4 {
		var b bytes.Buffer
		rep.Write(&b)
		t.Fatalf("prompt findings = %+v\n%s", rep.PromptQuality, b.String())
	}
	h := byTarget[TargetHint]
	if h.Collection != "datadog" || h.Sessions != 3 || len(h.Pages) != 1 || h.Pages[0] != "root:datadog" {
		t.Errorf("hint finding = %+v", h)
	}
	if h.Queries[0] != "datadgo alert routing" || len(h.Queries) != 2 {
		t.Errorf("hint queries = %v", h.Queries)
	}
	if strings.Join(h.Terms, " ") != "alert api datadgo key rotate routing" {
		t.Errorf("hint terms = %v", h.Terms)
	}
	d := byTarget[TargetDescription]
	if d.Collection != "flux" || d.Sessions != 2 {
		t.Errorf("description finding = %+v", d)
	}
	tl := byTarget[TargetTool]
	if tl.Collection != "root" || tl.Sessions != 2 || tl.Queries[0] != "who is on call" {
		t.Errorf("tool finding = %+v", tl)
	}
	r := byTarget[TargetRoute]
	if r.Collection != "root" || r.Sessions != 2 || len(r.Pages) != 2 {
		t.Errorf("route finding = %+v", r)
	}
	// The hint finding is the best supported and comes first.
	if rep.PromptQuality[0].Target != TargetHint {
		t.Errorf("order = %v", rep.PromptQuality)
	}
	var b bytes.Buffer
	rep.Write(&b)
	for _, want := range []string{"prompt_quality root:datadog", `"datadgo alert routing"`, "mk_search tool description", "do not separate its children", "fuzzy/prefix answered"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}
	// Raising the bar drops everything but the hint finding.
	rep, err = Librarian(ctx, reg, nil, LibrarianOpts{Log: log, Days: 2, PromptMinSessions: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.PromptQuality) != 1 || rep.PromptQuality[0].Target != TargetHint {
		t.Errorf("min sessions 3: %+v", rep.PromptQuality)
	}
}

func TestQueryTerms(t *testing.T) {
	got := strings.Join(queryTerms(`+Datadog -flux "API key" title:rotate how do I? id`), " ")
	if got != "datadog flux api key rotate" {
		t.Errorf("terms = %q", got)
	}
}
