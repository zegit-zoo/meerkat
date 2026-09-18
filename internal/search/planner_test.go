package search

import (
	"context"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

func hubPages() []kb.Page {
	return []kb.Page{
		{ID: "hub/datadog", Title: "Datadog", Body: "Datadog monitors, dashboards and the search of its own docs", Front: kb.Frontmatter{Type: "pointer", Status: "reviewed", Tags: []string{"vendor", "observability"}}},
		{ID: "hub/flux", Title: "Flux CD", Body: "GitOps, HelmRelease drift, kustomization", Front: kb.Frontmatter{Type: "pointer", Status: "reviewed", Tags: []string{"gitops"}}},
		{ID: "hub/pagerduty", Title: "PagerDuty", Body: "on-call schedules and escalation", Front: kb.Frontmatter{Type: "pointer", Status: "placeholder", Tags: []string{"vendor"}}},
		{ID: "notes/helm", Title: "Helm notes", Body: "helm charts and values", Front: kb.Frontmatter{Category: "notes", Subcategory: "packaging"}},
	}
}

func TestPlanner_ExactHitsStayExact(t *testing.T) {
	idx, err := NewFromPages(hubPages())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	res, stage, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), "datadog monitors", 10)
	if err != nil || stage != StageExact || len(res) == 0 || res[0].Page.ID != "hub/datadog" || res[0].Stage != StageExact {
		t.Fatalf("exact: res=%v stage=%s err=%v", ids(res), stage, err)
	}
}

// The acceptance case from meerkat-mob #3: a query with two typos and
// one exact term still routes to the Datadog pointer page.
func TestPlanner_TyposFallBackToFuzzy(t *testing.T) {
	idx, err := NewFromPages(hubPages())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	res, stage, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), "datadgo monitor serach", 10)
	if err != nil {
		t.Fatal(err)
	}
	if stage != StageFuzzy || len(res) == 0 || res[0].Page.ID != "hub/datadog" || res[0].Stage != StageFuzzy {
		t.Fatalf("fuzzy: res=%v stage=%s", ids(res), stage)
	}
	// Plain QueryAs sees the same results without the stage.
	plain, err := idx.QueryAs(context.Background(), kb.Unfiltered(), "datadgo monitor serach", 10)
	if err != nil || len(plain) != len(res) {
		t.Errorf("QueryAs = %v %v", ids(plain), err)
	}
}

func TestPlanner_ShortTermsFallBackToPrefix(t *testing.T) {
	idx, err := NewFromPages(hubPages())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	// "pager" is a prefix of "pagerduty" and no edit-distance-1 word.
	res, stage, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), "pager", 10)
	if err != nil {
		t.Fatal(err)
	}
	if stage != StagePrefix || len(res) != 1 || res[0].Page.ID != "hub/pagerduty" {
		t.Fatalf("prefix: res=%v stage=%s", ids(res), stage)
	}
	// Nothing at any stage: the deepest stage is reported with no hits.
	res, stage, err = idx.QueryStaged(context.Background(), kb.Unfiltered(), "zzzzzzzz", 10)
	if err != nil || len(res) != 0 || stage != StagePrefix {
		t.Errorf("miss: res=%v stage=%s err=%v", ids(res), stage, err)
	}
}

func TestPlanner_NeverRewritesAdvancedSyntax(t *testing.T) {
	idx, err := NewFromPages(hubPages())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for _, q := range []string{`title:datadgo`, `"datadgo monitor"`, `datadgo*`, `datadgo~1`, `+datadgo -flux`} {
		res, stage, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), q, 10)
		if err != nil {
			continue // bleve may reject a malformed query string; that is the exact stage's answer
		}
		if stage != StageExact {
			t.Errorf("%q: stage = %s, want exact only (no rewrite of author syntax); res=%v", q, stage, ids(res))
		}
	}
	if terms := fallbackTerms("Helm-Release drift-detektering helm"); strings.Join(terms, ",") != "helm,release,drift,detektering" {
		t.Errorf("fallbackTerms = %v", terms)
	}
}

func TestPlanner_FallbackRespectsVisibility(t *testing.T) {
	pages := append(hubPages(), kb.Page{ID: "memory/personal/alice/secret", Title: "Alice datadog secret", Body: "datadog api key rotation"})
	idx, err := NewFromPages(pages)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	res, _, err := idx.QueryStaged(context.Background(), kb.AsOwner("bob"), "datadgo rotaton", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if strings.Contains(r.Page.ID, "alice") {
			t.Fatalf("fuzzy stage leaked a private page: %v", ids(res))
		}
	}
}

func TestMatch_KeywordFieldsAreQueryClauses(t *testing.T) {
	idx, err := NewFromPages(hubPages())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()
	got, err := idx.Match(ctx, kb.Unfiltered(), Filters{Type: "pointer", Status: "reviewed"})
	if err != nil || len(got) != 2 || got[0].ID != "hub/datadog" || got[1].ID != "hub/flux" {
		t.Errorf("type+status = %v %v", pageIDs(got), err)
	}
	got, _ = idx.Match(ctx, kb.Unfiltered(), Filters{Tags: []string{"vendor", "observability"}})
	if len(got) != 1 || got[0].ID != "hub/datadog" {
		t.Errorf("all tags = %v", pageIDs(got))
	}
	got, _ = idx.Match(ctx, kb.Unfiltered(), Filters{Tags: []string{"vendor"}, Prefix: "hub/p"})
	if len(got) != 1 || got[0].ID != "hub/pagerduty" {
		t.Errorf("tag+prefix = %v", pageIDs(got))
	}
	got, _ = idx.Match(ctx, kb.Unfiltered(), Filters{Category: "notes", Subcategory: "packaging"})
	if len(got) != 1 || got[0].ID != "notes/helm" {
		t.Errorf("category+subcategory = %v", pageIDs(got))
	}
	got, _ = idx.Match(ctx, kb.Unfiltered(), Filters{})
	if len(got) != 4 {
		t.Errorf("empty filters = %v", pageIDs(got))
	}
	got, _ = idx.Match(ctx, kb.Unfiltered(), Filters{Status: "nope"})
	if len(got) != 0 {
		t.Errorf("no match = %v", pageIDs(got))
	}
}

func TestNgramAnalyzer_MatchesTitlePrefixesAndIsBounded(t *testing.T) {
	idx, err := NewFromPages(hubPages(), WithTitleAnalyzer(AnalyzerNgram))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	res, stage, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), "datadg", 10)
	if err != nil || len(res) == 0 || res[0].Page.ID != "hub/datadog" || stage != StageExact {
		t.Errorf("ngram title: res=%v stage=%s err=%v", ids(res), stage, err)
	}

	big := []kb.Page{{ID: "big", Title: "Big", Body: strings.Repeat("x", MaxNgramCorpusBytes+1)}}
	if _, err := NewFromPages(big, WithTitleAnalyzer(AnalyzerNgram)); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Errorf("oversized ngram corpus: err = %v", err)
	}
	if _, err := NewFromPages(hubPages(), WithTitleAnalyzer("soundex")); err == nil || !strings.Contains(err.Error(), "unknown title analyzer") {
		t.Errorf("unknown analyzer: err = %v", err)
	}
	std, err := NewFromPages(hubPages(), WithTitleAnalyzer(AnalyzerStandard))
	if err != nil {
		t.Fatal(err)
	}
	std.Close()
}

// syntheticCorpus builds n pages of realistic size so the planner
// benchmarks measure something: the embedded corpus in this repo is
// the empty placeholder.
func syntheticCorpus(n int) []kb.Page {
	words := []string{"rate", "limiting", "retries", "backoff", "datadog", "monitor", "search", "flux", "helmrelease", "drift", "kustomization", "gitops", "postgres", "backup", "restore", "garage", "bucket", "policy", "incident", "runbook"}
	pages := make([]kb.Page, 0, n)
	for i := 0; i < n; i++ {
		var body strings.Builder
		for j := 0; j < 120; j++ {
			body.WriteString(words[(i*7+j*3)%len(words)])
			body.WriteByte(' ')
		}
		pages = append(pages, kb.Page{
			ID:    "systems/" + words[i%len(words)] + "/" + words[(i*3)%len(words)] + "-" + strings.Repeat("x", i%5),
			Title: words[i%len(words)] + " " + words[(i*5)%len(words)] + " notes",
			Body:  body.String(),
			Front: kb.Frontmatter{Type: words[(i*11)%len(words)], Status: "reviewed"},
		})
	}
	return pages
}

func benchIndex(b *testing.B) *Index {
	b.Helper()
	idx, err := NewFromPages(syntheticCorpus(500))
	if err != nil {
		b.Fatal(err)
	}
	return idx
}

func BenchmarkQueryStagedExact(b *testing.B) {
	idx := benchIndex(b)
	defer idx.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), "rate limiting retries", 10); err != nil {
			b.Fatal(err)
		}
	}
}

// A miss at the exact stage: the fuzzy pass runs on top of it.
func BenchmarkQueryStagedFuzzy(b *testing.B) {
	idx := benchIndex(b)
	defer idx.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), "limting retreis", 10); err != nil {
			b.Fatal(err)
		}
	}
}

// A miss at both stages: exact, fuzzy and prefix all run.
func BenchmarkQueryStagedPrefix(b *testing.B) {
	idx := benchIndex(b)
	defer idx.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), "kus", 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMatch(b *testing.B) {
	idx := benchIndex(b)
	defer idx.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.Match(context.Background(), kb.Unfiltered(), Filters{Type: "flux", Status: "reviewed"}); err != nil {
			b.Fatal(err)
		}
	}
}

func ids(res []Result) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Page.ID
	}
	return out
}

func pageIDs(pages []kb.Page) []string {
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = p.ID
	}
	return out
}
