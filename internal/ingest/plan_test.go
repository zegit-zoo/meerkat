package ingest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestPlan_AllStaleProducesTasks: with no filters, the planner picks
// up every placeholder/ingest-failed page (i.e. the default "what
// needs work" set).
func TestPlan_AllStale(t *testing.T) {
	tasks, err := Plan(PlanOpts{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// We expect *some* placeholders in the embedded snapshot for at
	// least the lifetime of v0.x. If everything's been ingested this
	// will be 0; that's a real-world signal, not a bug.
	for _, task := range tasks {
		if task.Prompt == "" {
			t.Errorf("task %s has empty prompt", task.PageID)
		}
		if task.Model == "" {
			t.Errorf("task %s has empty model", task.PageID)
		}
		if task.PagePath == "" {
			t.Errorf("task %s has empty page_path", task.PageID)
		}
	}
}

// TestPlan_BySource narrows to one source.
func TestPlan_BySource(t *testing.T) {
	tasks, err := Plan(PlanOpts{SourceID: "policies"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, task := range tasks {
		if task.SourceID != "policies" {
			t.Errorf("task %s has source_id %q, want 'policies'",
				task.PageID, task.SourceID)
		}
		if !strings.HasPrefix(task.PageID, "policies/") {
			t.Errorf("task %s page_id should start with policies/", task.PageID)
		}
	}
}

// TestPlan_ByPage_Single: --page narrows to exactly one (or zero).
func TestPlan_ByPage_Single(t *testing.T) {
	// Plan-all to find any one stale page to target.
	all, err := Plan(PlanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Skip("no stale pages in this snapshot — skip")
	}
	pick := all[0].PageID

	tasks, err := Plan(PlanOpts{PageID: pick})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task for --page %s, got %d", pick, len(tasks))
	}
	if tasks[0].PageID != pick {
		t.Errorf("got %s, want %s", tasks[0].PageID, pick)
	}
}

// TestPlan_ByPage_NotFound returns no tasks for a non-existent page.
func TestPlan_ByPage_NotFound(t *testing.T) {
	tasks, err := Plan(PlanOpts{PageID: "does-not-exist/anywhere"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(tasks))
	}
}

// TestWriteJSONL: round-trip via JSON decoder.
func TestWriteJSONL(t *testing.T) {
	tasks := []Task{
		{
			PageID:        "policies/foo",
			PagePath:      "wiki/policies/foo.md",
			SourceID:      "policies",
			Prompt:        "do thing",
			Model:         "openai/gpt-5.5-fast",
			SubagentType:  "general",
			WallClockCapS: 300,
		},
		{
			PageID:        "policies/bar",
			PagePath:      "wiki/policies/bar.md",
			SourceID:      "policies",
			Prompt:        "do other thing",
			Model:         "openai/gpt-5.5-fast",
			SubagentType:  "general",
			WallClockCapS: 300,
		},
	}
	var buf bytes.Buffer
	if err := WriteJSONL(&buf, tasks); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&buf)
	for i := range tasks {
		var got Task
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("decode line %d: %v", i, err)
		}
		if got.PageID != tasks[i].PageID {
			t.Errorf("line %d: PageID = %q, want %q", i, got.PageID, tasks[i].PageID)
		}
	}
	// And no extra lines.
	var trailing Task
	if err := dec.Decode(&trailing); err == nil {
		t.Errorf("unexpected trailing line: %+v", trailing)
	}
}

// TestPlan_PromptSubstitution: rendered prompt has filled-in {{var}}s.
func TestPlan_PromptSubstitution(t *testing.T) {
	tasks, _ := Plan(PlanOpts{SourceID: "policies"})
	if len(tasks) == 0 {
		t.Skip("no policy placeholders to test against")
	}
	p := tasks[0].Prompt
	if strings.Contains(p, "{{page_id}}") {
		t.Error("prompt still has {{page_id}} marker")
	}
	if strings.Contains(p, "{{page_path}}") {
		t.Error("prompt still has {{page_path}} marker")
	}
	if !strings.Contains(p, tasks[0].PageID) {
		t.Error("prompt should embed the page id")
	}
}

// TestNormalisePageID: helper accepts the three forms callers use.
func TestNormalisePageID(t *testing.T) {
	cases := map[string]string{
		"policies/foo":         "policies/foo",
		"policies/foo.md":      "policies/foo",
		"wiki/policies/foo.md": "policies/foo",
		"content/policies/foo": "policies/foo",
	}
	for in, want := range cases {
		if got := normalisePageID(in); got != want {
			t.Errorf("normalisePageID(%q) = %q, want %q", in, got, want)
		}
	}
}
