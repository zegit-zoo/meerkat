package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// rewriteAgent edits the page the way a real run might: correctly
// (field only), badly (body too), or not at all.
type rewriteAgent struct{ mode string }

func (a rewriteAgent) Name() string { return "fake" }

func (a rewriteAgent) Exec(_ context.Context, t Task, env ExecEnv) Result {
	r := Result{Task: t, StartedAt: time.Now()}
	full := filepath.Join(env.WorkdirKB, filepath.FromSlash(t.PagePath))
	b, _ := os.ReadFile(full)
	p, err := kb.ParsePage(t.PageID, t.PagePath, b)
	if err != nil {
		r.FinishedAt, r.ExitStatus, r.FailReason = time.Now(), "failed", err.Error()
		return r
	}
	switch a.mode {
	case "good":
		if t.Field == FieldHint {
			p.Front.Hint = "Datadog: dashboards, monitors, alert routing, API keys and their rotation."
		} else {
			p.Front.Description = "Kustomization reconcile and HelmRelease drift in Flux."
		}
	case "body":
		p.Front.Hint = "Datadog alerts."
		p.Body += "\nExtra paragraph the agent should not have added.\n"
	case "other-field":
		p.Front.Hint = "Datadog alerts."
		p.Front.Title = "Renamed"
	case "noop":
	}
	if a.mode != "noop" {
		_ = os.WriteFile(full, renderPage(p), 0o600)
	}
	r.FinishedAt = time.Now()
	if reason := roleFailure(t, full, "fake"); reason != "" {
		r.ExitStatus, r.FailReason = "failed", reason
	} else {
		r.ExitStatus = "ok"
	}
	return r
}

func rewriteWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("wiki/datadog.md", "---\nid: datadog\ntitle: Datadog\ntype: pointer\ntarget: collection:datadog\nhint: Dashboards.\n---\n# Datadog\n\nGo there for dashboards.\n")
	write("wiki/concepts/drift.md", "---\nid: concepts/drift\ntitle: Drift\ntype: Concept\ndescription: Drift.\n---\n# Drift\n")
	return dir
}

func rewriteReport() *Report {
	return &Report{PromptQuality: []PromptFinding{
		{Target: TargetHint, Collection: "datadog", Pages: []string{"root:datadog"}, Queries: []string{"datadgo alert routing", "rotate the datadog api key"}, Sessions: 3, Terms: []string{"alert", "api", "datadgo", "key", "rotate", "routing"}},
		{Target: TargetDescription, Collection: "flux", Pages: []string{"flux:concepts/drift", "flux:concepts/elsewhere"}, Queries: []string{"kustomizaton reconcile"}, Sessions: 2},
		{Target: TargetTool, Collection: "root", Queries: []string{"who is on call"}, Sessions: 2},
	}}
}

func TestRewrites_PlanExecuteAndAcceptFieldOnlyEdits(t *testing.T) {
	ctx := context.Background()
	dir := rewriteWorkdir(t)
	rep := rewriteReport()
	tasks, skips, err := PlanRewrites(rep, RewritePlanOpts{Workdir: dir, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || len(skips) != 1 || !strings.Contains(skips[0].Reason, "not in this working copy") {
		t.Fatalf("tasks = %+v skips = %+v", tasks, skips)
	}
	hint := tasks[0]
	if hint.Role != RoleLibrarian || hint.Field != FieldHint || hint.PagePath != "wiki/datadog.md" || hint.TargetKB != "root" || hint.IntakeID != "rewrite:root:datadog" {
		t.Errorf("hint task = %+v", hint)
	}
	for _, want := range []string{"Field you may change: `hint:`", "- datadgo alert routing", "alert, api, datadgo", "under 300 characters"} {
		if !strings.Contains(hint.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	instr := buildInstruction(hint, dir, "librarian/rewrites")
	if !strings.Contains(instr, `git commit -m "librarian(rewrite): hint datadog for queries "datadgo alert routing", "rotate the datadog api key""`) || !strings.Contains(instr, "git push origin librarian/rewrites") {
		t.Errorf("instruction:\n%s", instr)
	}
	if tasks[1].Field != FieldDescription || tasks[1].PagePath != "wiki/concepts/drift.md" {
		t.Errorf("description task = %+v", tasks[1])
	}

	before, err := Snapshot(dir, tasks)
	if err != nil {
		t.Fatal(err)
	}
	results, err := Run(ctx, tasks, ExecOpts{WorkdirKB: dir, Executor: rewriteAgent{mode: "good"}})
	if err != nil {
		t.Fatal(err)
	}
	done, err := FinalizeRewrites(ctx, dir, results, before)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 || done[0].Action != "rewritten" || done[1].Action != "rewritten" {
		t.Fatalf("done = %+v", done)
	}
	if done[0].Before != "Dashboards." || !strings.Contains(done[0].After, "alert routing") || done[0].Page != "root:datadog" {
		t.Errorf("hint rewrite = %+v", done[0])
	}
	b, _ := os.ReadFile(filepath.Join(dir, "wiki/datadog.md"))
	if !strings.Contains(string(b), "hint: 'Datadog: dashboards") && !strings.Contains(string(b), "hint: Datadog: dashboards") && !strings.Contains(string(b), `hint: "Datadog: dashboards`) {
		t.Errorf("file not rewritten:\n%s", b)
	}
	if !strings.Contains(string(b), "Go there for dashboards.") {
		t.Errorf("body lost:\n%s", b)
	}

	// The tool finding becomes a proposal file, never a task.
	rel, err := WriteToolProposal(rep, dir, time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "ingestion/proposals/tool-description-2026-09-19.md" {
		t.Errorf("proposal at %q", rel)
	}
	pb, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if !strings.Contains(string(pb), `"who is on call"`) || !strings.Contains(string(pb), "never applied to a running server") {
		t.Errorf("proposal:\n%s", pb)
	}
	if rel, _ := WriteToolProposal(&Report{}, dir, time.Now()); rel != "" {
		t.Errorf("empty report wrote %q", rel)
	}
}

func TestRewrites_RejectsAnythingBeyondTheField(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ mode, action, detail string }{
		{"body", "rejected", "the body changed"},
		{"other-field", "rejected", "other than hint changed"},
		{"noop", "unchanged", "changed nothing"},
	} {
		dir := rewriteWorkdir(t)
		rep := rewriteReport()
		rep.PromptQuality = rep.PromptQuality[:1]
		tasks, _, err := PlanRewrites(rep, RewritePlanOpts{Workdir: dir})
		if err != nil {
			t.Fatal(err)
		}
		orig, _ := os.ReadFile(filepath.Join(dir, "wiki/datadog.md"))
		before, _ := Snapshot(dir, tasks)
		results, _ := Run(ctx, tasks, ExecOpts{WorkdirKB: dir, Executor: rewriteAgent{mode: tc.mode}})
		done, err := FinalizeRewrites(ctx, dir, results, before)
		if err != nil {
			t.Fatal(err)
		}
		if len(done) != 1 || done[0].Action != tc.action || !strings.Contains(done[0].Detail, tc.detail) {
			t.Errorf("%s: done = %+v", tc.mode, done)
		}
		after, _ := os.ReadFile(filepath.Join(dir, "wiki/datadog.md"))
		if string(after) != string(orig) {
			t.Errorf("%s: page not restored:\n%s", tc.mode, after)
		}
	}
}
