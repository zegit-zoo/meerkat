package ingest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// fakeAgent is an executor that does what a well-behaved researcher or
// validator would, without a model: it writes the candidate page or
// edits its frontmatter as told by mode.
type fakeAgent struct {
	mode string // "research" | "verify" | "fail" | "needs-human" | "noop"
	by   string
	runs int
}

func (f *fakeAgent) Name() string { return "fake" }

func (f *fakeAgent) Exec(_ context.Context, t Task, env ExecEnv) Result {
	f.runs++
	r := Result{Task: t, StartedAt: time.Now()}
	full := filepath.Join(env.WorkdirKB, filepath.FromSlash(t.PagePath))
	switch f.mode {
	case "research":
		raw, _ := os.ReadFile(filepath.Join(env.WorkdirKB, filepath.FromSlash(t.RawPath)))
		if !strings.Contains(string(raw), "Organization Settings") {
			r.FinishedAt, r.ExitStatus, r.FailReason = time.Now(), "failed", "raw item not materialised"
			return r
		}
		_ = os.MkdirAll(filepath.Dir(full), 0o750)
		_ = os.WriteFile(full, []byte("---\nid: "+t.PageID+"\ntitle: Rotate the Datadog API key\ntype: Runbook\nstatus: unverified\nsource:\n  urls: [https://example.com/doc]\nrelated: [flux:concepts/drift]\n---\n# Rotate\n\nOrganization Settings > API Keys.\n"), 0o600)
	case "verify", "fail", "needs-human":
		b, _ := os.ReadFile(full)
		p, err := kb.ParsePage(t.PageID, t.PagePath, b)
		if err != nil {
			r.FinishedAt, r.ExitStatus, r.FailReason = time.Now(), "failed", err.Error()
			return r
		}
		switch f.mode {
		case "verify":
			p.Front.Verified = append(p.Front.Verified, kb.Verifier{By: "agent:validator:" + f.by, At: time.Now().UTC().Format(time.RFC3339)})
		case "fail":
			p.Front.FailureReason = "claim 2 not in the cited source"
		case "needs-human":
			p.Front.FailureReason = "needs-human: this is a policy question"
		}
		_ = os.WriteFile(full, renderPage(p), 0o600)
	case "noop":
	}
	r.FinishedAt = time.Now()
	if reason := roleFailure(t, full, "fake"); reason != "" {
		r.ExitStatus, r.FailReason = "failed", reason
	} else {
		r.ExitStatus = "ok"
	}
	return r
}

func intakeFixture(t *testing.T) (*intake.Store, string) {
	t.Helper()
	ms, err := memory.OpenLocal(filepath.Join(t.TempDir(), "intake"))
	if err != nil {
		t.Fatal(err)
	}
	st := intake.New(ms)
	now := time.Date(2026, 9, 18, 20, 0, 0, 0, time.UTC)
	raw := []byte(`---
{"id":"it1","type":"research-raw","status":"unverified","source":"agent-fallback","outcome":"gave_up","fallback_kind":"web","question":"how do I rotate the datadog api key","attempted":["root","platform","root/platform/flux"],"reported_at":"2026-09-18T20:00:00Z","submitted_by":"alice-ns","fallback_sources":["https://example.com/doc"]}
---
# Research

Rotate it in Organization Settings > API Keys.
`)
	if _, err := st.PutRaw(context.Background(), "alice-ns", now, "it1", raw); err != nil {
		t.Fatal(err)
	}
	none := []byte(`---
{"id":"it2","type":"research-raw","outcome":"found","fallback_kind":"none","question":"x","reported_at":"2026-09-18T20:01:00Z"}
---
`)
	if _, err := st.PutRaw(context.Background(), "alice-ns", now, "it2", none); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	return st, workdir
}

func TestResearcher_RawItemBecomesStagedCandidate(t *testing.T) {
	ctx := context.Background()
	st, workdir := intakeFixture(t)
	opts := IntakePlanOpts{Role: RoleResearcher, Workdir: workdir, Model: "model-a"}
	tasks, skips, err := PlanIntake(ctx, st, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || len(skips) != 1 || tasks[0].IntakeID != "it1" || tasks[0].TargetKB != "flux" || tasks[0].PagePath != "wiki/intake/it1.md" {
		t.Fatalf("plan = %+v skips=%+v", tasks, skips)
	}
	if !strings.Contains(tasks[0].Prompt, "how do I rotate the datadog api key") || !strings.Contains(tasks[0].Prompt, "ingestion/intake/it1.md") {
		t.Errorf("prompt not rendered: %s", tasks[0].Prompt[:200])
	}
	agent := &fakeAgent{mode: "research"}
	results, err := Run(ctx, tasks, ExecOpts{WorkdirKB: workdir, Executor: agent, Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	fins, err := Finalize(ctx, st, workdir, results, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(fins) != 1 || fins[0].Action != "staged" || fins[0].Detail != "staged/flux/it1.md" {
		t.Fatalf("finalize = %+v", fins)
	}
	// The staged page carries provenance and the intake id.
	staged, _ := st.ListStaged(ctx)
	p, err := kb.ParsePage("intake/it1", "x", staged[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if p.Front.Extra["intake_id"] != "it1" || p.Front.Extra["researcher_model"] != "model-a" || p.Front.Generated == nil || p.Front.LastIngested == "" || p.Front.Status != "unverified" {
		t.Errorf("stamped frontmatter = %+v", p.Front)
	}
	// Idempotent: a second plan finds nothing to do.
	tasks, _, _ = PlanIntake(ctx, st, opts)
	if len(tasks) != 0 {
		t.Errorf("re-run planned %d tasks; the done marker must make it a no-op", len(tasks))
	}
}

func TestValidator_TwoIndependentConfirmationsThenFile(t *testing.T) {
	ctx := context.Background()
	st, workdir := intakeFixture(t)
	research := IntakePlanOpts{Role: RoleResearcher, Workdir: workdir, Model: "model-a"}
	tasks, _, _ := PlanIntake(ctx, st, research)
	results, _ := Run(ctx, tasks, ExecOpts{WorkdirKB: workdir, Executor: &fakeAgent{mode: "research"}, Out: os.Stderr, Err: os.Stderr})
	if _, err := Finalize(ctx, st, workdir, results, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The researcher's model may not validate its own work.
	_, skips, err := PlanIntake(ctx, st, IntakePlanOpts{Role: RoleValidator, Workdir: workdir, Model: "model-a"})
	if err != nil || len(skips) != 1 || !strings.Contains(skips[0].Reason, "researcher's model") {
		t.Fatalf("same-model validator: skips=%+v err=%v", skips, err)
	}

	validate := func(by string) []Finalization {
		t.Helper()
		tasks, _, err := PlanIntake(ctx, st, IntakePlanOpts{Role: RoleValidator, Workdir: workdir, Model: "model-b"})
		if err != nil || len(tasks) != 1 {
			t.Fatalf("plan validation = %d %v", len(tasks), err)
		}
		results, _ := Run(ctx, tasks, ExecOpts{WorkdirKB: workdir, Executor: &fakeAgent{mode: "verify", by: by}, Out: os.Stderr, Err: os.Stderr})
		fins, err := Finalize(ctx, st, workdir, results, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return fins
	}
	if fins := validate("one"); fins[0].Action != "pending" || !strings.Contains(fins[0].Detail, "1 of 2") {
		t.Fatalf("first validation = %+v", fins)
	}
	if fins := validate("two"); fins[0].Action != "confirmed" {
		t.Fatalf("second validation = %+v", fins)
	}
	if done, _ := st.IsDone(ctx, "validated-it1"); !done {
		t.Error("confirmed candidate must be marked validated")
	}
	if tasks, skips, _ := PlanIntake(ctx, st, IntakePlanOpts{Role: RoleValidator, Workdir: workdir, Model: "model-c"}); len(tasks) != 0 || len(skips) != 1 {
		t.Errorf("a confirmed candidate must not be planned again: %d %+v", len(tasks), skips)
	}
}

func TestValidator_DisagreementParksAndNeedsHumanParks(t *testing.T) {
	ctx := context.Background()
	st, workdir := intakeFixture(t)
	tasks, _, _ := PlanIntake(ctx, st, IntakePlanOpts{Role: RoleResearcher, Workdir: workdir, Model: "model-a"})
	results, _ := Run(ctx, tasks, ExecOpts{WorkdirKB: workdir, Executor: &fakeAgent{mode: "research"}, Out: os.Stderr, Err: os.Stderr})
	if _, err := Finalize(ctx, st, workdir, results, time.Now()); err != nil {
		t.Fatal(err)
	}
	run := func(mode string) Finalization {
		t.Helper()
		tasks, _, err := PlanIntake(ctx, st, IntakePlanOpts{Role: RoleValidator, Workdir: workdir, Model: "model-b"})
		if err != nil || len(tasks) != 1 {
			t.Fatalf("plan = %d %v", len(tasks), err)
		}
		results, _ := Run(ctx, tasks, ExecOpts{WorkdirKB: workdir, Executor: &fakeAgent{mode: mode}, Out: os.Stderr, Err: os.Stderr})
		fins, err := Finalize(ctx, st, workdir, results, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return fins[0]
	}
	if f := run("fail"); f.Action != "pending" || !strings.Contains(f.Detail, "1 of 2") {
		t.Fatalf("first failure = %+v", f)
	}
	if f := run("fail"); f.Action != "parked" || !strings.Contains(f.Detail, "disagreed 2 times") {
		t.Fatalf("second failure = %+v", f)
	}
	parked, _ := st.Parked(ctx)
	if _, ok := parked["it1"]; !ok {
		t.Error("it1 must be parked")
	}

	// A fresh item flagged needs-human parks at once.
	st2, workdir2 := intakeFixture(t)
	tasks, _, _ = PlanIntake(ctx, st2, IntakePlanOpts{Role: RoleResearcher, Workdir: workdir2, Model: "model-a"})
	results, _ = Run(ctx, tasks, ExecOpts{WorkdirKB: workdir2, Executor: &fakeAgent{mode: "research"}, Out: os.Stderr, Err: os.Stderr})
	_, _ = Finalize(ctx, st2, workdir2, results, time.Now())
	tasks, _, _ = PlanIntake(ctx, st2, IntakePlanOpts{Role: RoleValidator, Workdir: workdir2, Model: "model-b"})
	results, _ = Run(ctx, tasks, ExecOpts{WorkdirKB: workdir2, Executor: &fakeAgent{mode: "needs-human"}, Out: os.Stderr, Err: os.Stderr})
	fins, _ := Finalize(ctx, st2, workdir2, results, time.Now())
	if fins[0].Action != "parked" || !strings.Contains(fins[0].Detail, "policy question") {
		t.Errorf("needs-human = %+v", fins[0])
	}

	// An executor that does nothing is a failure, not a silent pass.
	st3, workdir3 := intakeFixture(t)
	tasks, _, _ = PlanIntake(ctx, st3, IntakePlanOpts{Role: RoleResearcher, Workdir: workdir3, Model: "model-a"})
	results, _ = Run(ctx, tasks, ExecOpts{WorkdirKB: workdir3, Executor: &fakeAgent{mode: "noop"}, Out: os.Stderr, Err: os.Stderr})
	if results[0].ExitStatus != "failed" || !strings.Contains(results[0].FailReason, "no candidate page") {
		t.Errorf("noop researcher = %+v", results[0])
	}
}

// TestFinalize_SymlinkOutOfWorkingCopyIsRefused proves the containment in
// Finalize is the operating system's, not a string comparison. A symlink
// planted inside the working copy — by the very agent run whose results are
// being finalized — that points at a directory outside it passes
// isPathWithinBase, because filepath.Abs/Rel never resolve links. The os.Root
// the candidate read and both candidate writes go through refuses it, so the
// file outside is neither read into the intake store nor overwritten.
func TestFinalize_SymlinkOutOfWorkingCopyIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege we can't assume on windows")
	}
	ctx := context.Background()
	st, _ := intakeFixture(t)
	before, _ := st.ListStaged(ctx)

	workdir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "it1.md")
	const original = "---\nid: secret\ntitle: Not Yours\ntype: Runbook\nstatus: verified\n---\n# Secret\n"
	if err := os.WriteFile(victim, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workdir, "wiki"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workdir, "wiki", "intake")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	const pagePath = "wiki/intake/it1.md"
	// Precondition: the cheap lexical pre-check sees nothing wrong with this
	// path. That is exactly why os.Root has to be the gate behind it.
	if !isPathWithinBase(workdir, filepath.Join(workdir, filepath.FromSlash(pagePath))) {
		t.Fatal("precondition: isPathWithinBase is expected to accept the symlinked path")
	}

	results := []Result{{
		Task:       Task{Role: RoleResearcher, IntakeID: "it1", TargetKB: "flux", PageID: "intake/it1", PagePath: pagePath},
		ExitStatus: "ok",
	}}
	fins, err := Finalize(ctx, st, workdir, results, time.Now())
	if err != nil {
		t.Fatalf("Finalize errored instead of failing the one item: %v", err)
	}
	if len(fins) != 1 || fins[0].Action != "failed" || !strings.Contains(fins[0].Detail, "escapes the working copy") {
		t.Fatalf("finalize = %+v; want one failed item naming the working-copy escape", fins)
	}
	if got, _ := os.ReadFile(victim); string(got) != original {
		t.Errorf("a file outside the working copy was rewritten:\n%s", got)
	}
	if after, _ := st.ListStaged(ctx); len(after) != len(before) {
		t.Errorf("content from outside the working copy was staged: %d -> %d", len(before), len(after))
	}

	// The lexical pre-check still catches the plain ".." case on its own.
	results[0].Task.PagePath = "../" + filepath.Base(outside) + "/it1.md"
	fins, err = Finalize(ctx, st, workdir, results, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(fins) != 1 || fins[0].Action != "failed" || !strings.Contains(fins[0].Detail, "escapes the working copy") {
		t.Fatalf("dot-dot path = %+v; want a failed item naming the working-copy escape", fins)
	}
	if got, _ := os.ReadFile(victim); string(got) != original {
		t.Errorf("a file outside the working copy was rewritten via \"..\":\n%s", got)
	}
}

func TestRolePrompts_BuiltInsAndParse(t *testing.T) {
	for _, r := range []Role{RoleResearcher, RoleValidator, RoleLibrarian} {
		p, err := RolePrompt(r)
		if err != nil || !strings.Contains(p, string(r)) {
			t.Errorf("%s prompt: %v", r, err)
		}
	}
	if _, err := ParseRole("janitor"); err == nil {
		t.Error("unknown role accepted")
	}
	if r, err := ParseRole(" Validator "); err != nil || r != RoleValidator {
		t.Errorf("ParseRole = %q %v", r, err)
	}
	if _, _, err := PlanIntake(context.Background(), nil, IntakePlanOpts{Role: RoleResearcher, Workdir: "/x"}); err == nil {
		t.Error("no store must be an error")
	}
}
