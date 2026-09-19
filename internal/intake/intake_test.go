package intake

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/memory"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	ms, err := memory.OpenLocal(filepath.Join(t.TempDir(), "intake"))
	if err != nil {
		t.Fatal(err)
	}
	return New(ms)
}

func rawPage(id, ns, question, kind string) []byte {
	return []byte(`---
{"id":"` + id + `","type":"research-raw","status":"unverified","source":"agent-fallback","outcome":"gave_up","fallback_kind":"` + kind + `","question":"` + question + `","attempted":["root","platform","flux"],"reported_at":"2026-09-18T20:00:00Z","submitted_by":"` + ns + `","fallback_sources":["https://example.com/doc"]}
---
# Research: ` + question + `

Rotate it in Organization Settings.
`)
}

func TestKeysAndLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 9, 18, 20, 0, 0, 0, time.UTC)
	if got := RawKey("", now, "abc"); got != "raw/anonymous/2026-09-18/abc/page.md" {
		t.Errorf("RawKey anonymous = %q", got)
	}
	k1, err := st.PutRaw(ctx, "alice-ns", now, "id1", rawPage("id1", "alice-ns", "how do I rotate the key", "web"))
	if err != nil || k1 != "raw/alice-ns/2026-09-18/id1/page.md" {
		t.Fatalf("PutRaw = %q %v", k1, err)
	}
	if _, err := st.PutRaw(ctx, "bob-ns", now, "id2", rawPage("id2", "bob-ns", "what is drift", "source")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRaw(ctx, "alice-ns", now, "id1", []byte("dup")); err == nil {
		t.Error("a second deposit under the same key must be refused (create-only)")
	}

	all, err := st.ListRaw(ctx, "", false)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListRaw(all) = %d %v", len(all), err)
	}
	alice, _ := st.ListRaw(ctx, "alice-ns", false)
	if len(alice) != 1 || alice[0].ID != "id1" || alice[0].Namespace != "alice-ns" || alice[0].Question != "how do I rotate the key" || alice[0].FallbackKind != "web" || len(alice[0].Attempted) != 3 || len(alice[0].Sources) != 1 || alice[0].ReportedAt.IsZero() {
		t.Errorf("alice's items = %+v", alice)
	}
	if !strings.Contains(alice[0].Body, "Organization Settings") {
		t.Errorf("body = %q", alice[0].Body)
	}

	if err := st.MarkDone(ctx, "id1", "staged as x"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDone(ctx, "id1", "again"); err != nil {
		t.Errorf("MarkDone must be idempotent: %v", err)
	}
	if done, _ := st.IsDone(ctx, "id1"); !done {
		t.Error("id1 must be done")
	}
	rest, _ := st.ListRaw(ctx, "", false)
	if len(rest) != 1 || rest[0].ID != "id2" {
		t.Errorf("after done: %+v", rest)
	}
	withDone, _ := st.ListRaw(ctx, "", true)
	if len(withDone) != 2 {
		t.Errorf("includeDone: %d", len(withDone))
	}

	key, err := st.PutStaged(ctx, "flux", "id1", []byte("---\nid: intake/id1\n---\n# c\n"))
	if err != nil || key != "staged/flux/id1.md" {
		t.Fatalf("PutStaged = %q %v", key, err)
	}
	if k2, err := st.PutStaged(ctx, "flux", "id1", []byte("other")); err != nil || k2 != key {
		t.Errorf("staging twice is skipped, not an error: %q %v", k2, err)
	}
	staged, _ := st.ListStaged(ctx)
	if len(staged) != 1 || staged[0].KB != "flux" || staged[0].ID != "id1" || !strings.Contains(string(staged[0].Body), "# c") {
		t.Errorf("staged = %+v", staged)
	}

	if err := st.Park(ctx, "id2", "validators disagreed"); err != nil {
		t.Fatal(err)
	}
	parked, _ := st.Parked(ctx)
	if parked["id2"] != "validators disagreed" {
		t.Errorf("parked = %v", parked)
	}

	var none *Store
	if items, err := none.ListRaw(ctx, "", false); items != nil || err != nil {
		t.Error("nil store lists nothing")
	}
	if _, err := none.PutRaw(ctx, "x", now, "y", nil); err == nil {
		t.Error("nil store cannot deposit")
	}
}

func TestParse_Rejects(t *testing.T) {
	for _, body := range []string{"no frontmatter", "---\n{\"id\":\"x\"}\n", "---\nnot json\n---\nbody"} {
		if _, err := Parse("k", []byte(body)); err == nil {
			t.Errorf("Parse(%q) accepted", body)
		}
	}
}
