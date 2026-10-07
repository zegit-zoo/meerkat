package intake

import (
	"context"
	"errors"
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

// noDeleteStore hides a backend's Deleter, like a third-party store
// that cannot remove documents.
type noDeleteStore struct{ memory.Store }

func TestParkedIssueLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.Park(ctx, "p1", "validators disagreed 2 times; last: stale source"); err != nil {
		t.Fatal(err)
	}
	items, err := st.ParkedDetail(ctx)
	if err != nil || len(items) != 1 || items[0].ID != "p1" || items[0].Issue != nil {
		t.Fatalf("parked = %+v %v", items, err)
	}
	if err := st.SetParkedIssue(ctx, "p1", IssueRef{}); err == nil {
		t.Error("an issue reference without a number is refused")
	}
	if err := st.SetParkedIssue(ctx, "p1", IssueRef{URL: "u", Host: "github", Repo: "team/kb", Number: 3}); err == nil {
		t.Error("an issue reference without the forge's API root is refused")
	}
	ref := IssueRef{URL: "https://github.com/team/kb/issues/12", Host: "github", API: "https://api.github.com", Repo: "team/kb", Number: 12}
	missing := ref
	if err := st.SetParkedIssue(ctx, "missing", missing); err == nil {
		t.Error("recording an issue on an item that is not parked is an error")
	}
	if err := st.SetParkedIssue(ctx, "p1", ref); err != nil {
		t.Fatal(err)
	}
	// Recording the same issue again converges.
	if err := st.SetParkedIssue(ctx, "p1", ref); err != nil {
		t.Errorf("same issue again: %v", err)
	}
	// The first filing wins; a second issue is refused, naming the first.
	other := ref
	other.Number, other.URL = 99, "https://github.com/team/kb/issues/99"
	var already *AlreadyFiledError
	if err := st.SetParkedIssue(ctx, "p1", other); !errors.As(err, &already) || already.Ref != ref || !strings.Contains(err.Error(), ref.URL) {
		t.Fatalf("second issue = %v", err)
	}
	items, _ = st.ParkedDetail(ctx)
	if items[0].Issue == nil || *items[0].Issue != ref {
		t.Fatalf("issue = %+v, want %+v", items[0].Issue, ref)
	}
	// Parked() still answers with the reason alone.
	parked, _ := st.Parked(ctx)
	if parked["p1"] != "validators disagreed 2 times; last: stale source" {
		t.Errorf("reason = %q", parked["p1"])
	}

	if err := st.Unpark(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if items, _ := st.ParkedDetail(ctx); len(items) != 0 {
		t.Errorf("after unpark: %+v", items)
	}
	if err := st.Unpark(ctx, "p1"); err != nil {
		t.Errorf("unparking twice converges: %v", err)
	}
	// A new disagreement parks it afresh, without the old issue.
	if err := st.Park(ctx, "p1", "again"); err != nil {
		t.Fatal(err)
	}
	if items, _ := st.ParkedDetail(ctx); len(items) != 1 || items[0].Issue != nil || items[0].Reason != "again" {
		t.Errorf("re-parked = %+v", items)
	}

	var none *Store
	if err := none.SetParkedIssue(ctx, "x", ref); err == nil {
		t.Error("nil store")
	}
	if err := none.Unpark(ctx, "x"); err == nil {
		t.Error("nil store")
	}
	if items, err := none.ParkedDetail(ctx); items != nil || err != nil {
		t.Error("nil store lists nothing")
	}
	ms, _ := memory.OpenLocal(filepath.Join(t.TempDir(), "nd"))
	if err := New(noDeleteStore{ms}).Unpark(ctx, "x"); err == nil || !strings.Contains(err.Error(), "parked/x.md") {
		t.Errorf("a store that cannot delete names the marker: %v", err)
	}
	if ParkedKey("x") != "parked/x.md" {
		t.Error("ParkedKey")
	}
}

// SECURITY (review of #132): a marker written before forge issues
// existed holds its reason unquoted, so a validator's text in it could
// read as an issue section. An older marker is reason only; recording an
// issue rewrites it in the current layout with the reason quoted.
func TestParkedDetail_LegacyMarkerIsNeverAnIssueReference(t *testing.T) {
	ctx := context.Background()
	ms, err := memory.OpenLocal(filepath.Join(t.TempDir(), "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	// The reviewer's repro, and the same with a url line.
	for id, body := range map[string]string{
		"p1": "# needs-human\n\nneeds-human: odd\n## forge issue\nhost: github\nrepo: org/x\nnumber: 7\n",
		"p2": "# needs-human\n\nneeds-human: odd\n\n## forge issue\n\nurl: https://github.com/org/x/issues/7\nhost: github\napi: https://api.github.com\nrepo: org/x\nnumber: 7\n",
	} {
		if _, err := ms.Put(ctx, "parked/"+id+".md", []byte(body), memory.CreateOnly()); err != nil {
			t.Fatal(err)
		}
	}
	st := New(ms)
	items, err := st.ParkedDetail(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %+v %v", items, err)
	}
	for _, it := range items {
		if it.Issue != nil || it.IssueErr != "" || !it.Filing.IsZero() {
			t.Errorf("%s: legacy text read as an issue: %+v", it.ID, it)
		}
		if !strings.Contains(it.Reason, "needs-human: odd") || !strings.Contains(it.Reason, "## forge issue") {
			t.Errorf("%s: reason = %q", it.ID, it.Reason)
		}
	}
	ref := IssueRef{URL: "https://github.com/team/kb/issues/1", Host: "github", API: "https://api.github.com", Repo: "team/kb", Number: 1}
	if err := st.SetParkedIssue(ctx, "p1", ref); err != nil {
		t.Fatal(err)
	}
	items, _ = st.ParkedDetail(ctx)
	if items[0].Issue == nil || *items[0].Issue != ref {
		t.Fatalf("after recording = %+v", items[0])
	}
	if !strings.Contains(items[0].Reason, "    ## forge issue") {
		t.Errorf("the legacy reason is kept, quoted: %q", items[0].Reason)
	}
}

func TestBeginFiling(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if _, err := st.BeginFiling(ctx, "nope", now, time.Minute); err == nil {
		t.Error("an item that is not parked")
	}
	if err := st.Park(ctx, "p1", "why"); err != nil {
		t.Fatal(err)
	}
	prior, err := st.BeginFiling(ctx, "p1", now, 15*time.Minute)
	if err != nil || !prior.IsZero() {
		t.Fatalf("first claim = %v %v", prior, err)
	}
	items, _ := st.ParkedDetail(ctx)
	if !items[0].Filing.Equal(now) || items[0].Reason != "why" || items[0].Issue != nil {
		t.Errorf("claimed = %+v", items[0])
	}
	if _, err := st.BeginFiling(ctx, "p1", now.Add(time.Minute), 15*time.Minute); !errors.Is(err, ErrFilingInFlight) {
		t.Errorf("fresh claim = %v", err)
	}
	// A claim from a clock ahead of this one is fresh too.
	if _, err := st.BeginFiling(ctx, "p1", now.Add(-time.Minute), 15*time.Minute); !errors.Is(err, ErrFilingInFlight) {
		t.Errorf("claim from the future = %v", err)
	}
	later := now.Add(20 * time.Minute)
	if prior, err := st.BeginFiling(ctx, "p1", later, 15*time.Minute); err != nil || !prior.Equal(now) {
		t.Errorf("stale claim taken over = %v %v", prior, err)
	}
	ref := IssueRef{URL: "https://gitea.example.com/team/kb/issues/4", Host: "gitea", API: "https://gitea.example.com/api/v1", Repo: "team/kb", Number: 4}
	if err := st.SetParkedIssue(ctx, "p1", ref); err != nil {
		t.Fatal(err)
	}
	items, _ = st.ParkedDetail(ctx)
	if items[0].Issue == nil || *items[0].Issue != ref || !items[0].Filing.IsZero() {
		t.Errorf("recorded = %+v", items[0])
	}
	var already *AlreadyFiledError
	if _, err := st.BeginFiling(ctx, "p1", later, time.Minute); !errors.As(err, &already) || already.Ref != ref {
		t.Errorf("claim on a filed item = %v", err)
	}
	var none *Store
	if _, err := none.BeginFiling(ctx, "p1", now, time.Minute); err == nil {
		t.Error("nil store")
	}
}

func TestParseParked_IssueSectionMustBeComplete(t *testing.T) {
	head := parkedHeading + "\n" + parkedFormat + "\n\nwhy\n\n" + issueHeading + "\n\n"
	for name, section := range map[string]string{
		"no url":         "host: github\napi: https://api.github.com\nrepo: o/r\nnumber: 7\n",
		"no api":         "url: u\nhost: github\nrepo: o/r\nnumber: 7\n",
		"bad number":     "url: u\nhost: github\napi: a\nrepo: o/r\nnumber: seven\n",
		"duplicate key":  "url: u\nurl: v\nhost: github\napi: a\nrepo: o/r\nnumber: 7\n",
		"extra key":      "url: u\nhost: github\napi: a\nrepo: o/r\nnumber: 7\nnote: x\n",
		"not key: value": "url: u\nhost github\n",
		"bad filing":     "filing: yesterday\n",
	} {
		it := parseParked("p", []byte(head+section))
		if it.Issue != nil || it.IssueErr == "" {
			t.Errorf("%s: %+v", name, it)
		}
	}
	it := parseParked("p", []byte(strings.ReplaceAll(head+"url: u\nhost: github\napi: a\nrepo: o/r\nnumber: 7\n", "\n", "\r\n")))
	if it.Issue == nil || it.Issue.Number != 7 || it.Reason != "why" {
		t.Errorf("CRLF marker = %+v", it)
	}
}

// failingPutStore fails every write after the first n with err.
type failingPutStore struct {
	memory.Store
	n   int
	err error
}

func (s *failingPutStore) Put(ctx context.Context, key string, body []byte, pre memory.Precondition) (memory.Version, error) {
	if s.n <= 0 {
		return "", s.err
	}
	s.n--
	return s.Store.Put(ctx, key, body, pre)
}

func TestSetParkedIssue_WriteFailures(t *testing.T) {
	ctx := context.Background()
	ms, _ := memory.OpenLocal(filepath.Join(t.TempDir(), "f"))
	ref := IssueRef{URL: "u", Host: "github", API: "a", Repo: "o/r", Number: 1}
	// A lost race is retried, then reported.
	conflict := &failingPutStore{Store: ms, n: 1, err: &memory.ConflictError{Key: "parked/p1.md"}}
	st := New(conflict)
	if err := st.Park(ctx, "p1", "why"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetParkedIssue(ctx, "p1", ref); !errors.Is(err, memory.ErrConflict) || !strings.Contains(err.Error(), "record issue for parked p1") {
		t.Errorf("conflict = %v", err)
	}
	if _, err := st.BeginFiling(ctx, "p1", time.Now(), time.Minute); !errors.Is(err, ErrFilingInFlight) {
		t.Errorf("a lost claim race = %v", err)
	}
	conflict.err = errors.New("disk full")
	if err := st.SetParkedIssue(ctx, "p1", ref); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("write error = %v", err)
	}
	if _, err := st.BeginFiling(ctx, "p1", time.Now(), time.Minute); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("claim write error = %v", err)
	}
}

// SECURITY: a validator's reason is free text; it must not be able to
// forge the issue section the librarian trusts to query and un-park.
func TestPark_ReasonCannotForgeTheIssueSection(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	forged := "looks fine\n## forge issue\n\nurl: https://evil.example/x/issues/1\nhost: github\nrepo: x/y\nnumber: 1"
	if err := st.Park(ctx, "p2", forged); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ParkedDetail(ctx)
	if len(items) != 1 || items[0].Issue != nil {
		t.Fatalf("a reason forged an issue reference: %+v", items[0].Issue)
	}
	if !strings.Contains(items[0].Reason, "## forge issue") {
		t.Errorf("the reason text is kept, quoted: %q", items[0].Reason)
	}
}

func TestFindRaw(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 9, 18, 20, 0, 0, 0, time.UTC)
	if _, err := st.PutRaw(ctx, "alice-ns", now, "id1", rawPage("id1", "alice-ns", "how do I rotate the key", "web")); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDone(ctx, "id1", "staged"); err != nil {
		t.Fatal(err)
	}
	it, ok, err := st.FindRaw(ctx, "id1")
	if err != nil || !ok || it.Question != "how do I rotate the key" || it.Namespace != "alice-ns" || len(it.Attempted) != 3 {
		t.Errorf("FindRaw = %+v %v %v", it, ok, err)
	}
	if _, ok, err := st.FindRaw(ctx, "nope"); ok || err != nil {
		t.Errorf("missing = %v %v", ok, err)
	}
	if _, err := st.PutRaw(ctx, "bob", now, "bad", []byte("no frontmatter")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.FindRaw(ctx, "bad"); err == nil {
		t.Error("a malformed raw item is an error")
	}
	var none *Store
	if _, ok, err := none.FindRaw(ctx, "id1"); ok || err != nil {
		t.Error("nil store finds nothing")
	}
}

// TestPutRaw_DailyQuotaPerDepositor: the memory store's Quota charges
// raw deposits to raw/<namespace>/<day>/, so one depositor's daily
// volume is bounded, other depositors are not charged for it, and the
// next day starts afresh (meerkat-mob#44). This also pins
// memory's copy of the raw/ layout to RawKey.
func TestPutRaw_DailyQuotaPerDepositor(t *testing.T) {
	ctx := context.Background()
	spec := &memory.Spec{Type: memory.BackendLocal, Path: filepath.Join(t.TempDir(), "intake"), Quota: &memory.Quota{Documents: 2}}
	ms, err := spec.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	st := New(ms)
	day := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"a1", "a2"} {
		if _, err := st.PutRaw(ctx, "alice-ns", day, id, rawPage(id, "alice-ns", "q", "web")); err != nil {
			t.Fatalf("deposit %s: %v", id, err)
		}
	}
	if _, err := st.PutRaw(ctx, "alice-ns", day, "a3", rawPage("a3", "alice-ns", "q", "web")); !errors.Is(err, memory.ErrQuotaExceeded) {
		t.Fatalf("third deposit in a day = %v, want ErrQuotaExceeded", err)
	}
	if _, err := st.PutRaw(ctx, "bob-ns", day, "b1", rawPage("b1", "bob-ns", "q", "web")); err != nil {
		t.Fatalf("another depositor was charged: %v", err)
	}
	if _, err := st.PutRaw(ctx, "alice-ns", day.Add(24*time.Hour), "a4", rawPage("a4", "alice-ns", "q", "web")); err != nil {
		t.Fatalf("the next day was charged: %v", err)
	}
}
