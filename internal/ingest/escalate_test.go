package ingest

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/forge"
	"github.com/zegit-zoo/meerkat/internal/forge/forgetest"
	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// escalate_test.go is meerkat-mob #19's acceptance: a parked item
// produces exactly one issue with the summary; a second run files
// nothing; closing the issue with `resolved` un-parks the item.

const escalateTokenEnv = "MEERKAT_TEST_FORGE_TOKEN"

// escalateFixture: collection "handbook" with a github merge-request
// contract naming a token_env, a parked candidate for it with its raw
// deposit, and a contract-less collection "notes" with a parked
// candidate of its own.
func escalateFixture(t *testing.T, tokenEnv string) (*collections.Registry, *intake.Store) {
	t.Helper()
	reg, st, _ := escalateFixtureOn(t, tokenEnv)
	return reg, st
}

// escalateFixtureOn is escalateFixture that also returns the backing
// local store, for tests that wrap it or edit a marker by hand.
func escalateFixtureOn(t *testing.T, tokenEnv string) (*collections.Registry, *intake.Store, *memory.LocalStore) {
	t.Helper()
	ctx := context.Background()
	handbook := collections.FromPages("handbook", []kb.Page{{ID: "intro", Title: "Intro"}})
	handbook.Source.Update = &contentsource.UpdateSpec{Method: contentsource.UpdateMergeRequest, Repo: "https://github.com/example-org/handbook.git", Host: "github", TokenEnv: tokenEnv}
	handbook.Source.Update.Normalize()
	notes := collections.FromPages("notes", []kb.Page{{ID: "n", Title: "N"}})
	reg, err := collections.New(handbook, notes)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := memory.OpenLocal(filepath.Join(t.TempDir(), "intake"))
	if err != nil {
		t.Fatal(err)
	}
	st := intake.New(ms)
	raw := []byte(`---
{"id":"hb1","question":"how do we rotate @everyone's deploy key","attempted":["root","handbook"],"target_kb":"handbook","outcome":"gave_up","reported_at":"2026-09-18T20:00:00Z"}
---
# Research
`)
	if _, err := st.PutRaw(ctx, "alice", time.Date(2026, 9, 18, 20, 0, 0, 0, time.UTC), "hb1", raw); err != nil {
		t.Fatal(err)
	}
	cand := "---\nid: intake/hb1\ntitle: Rotate the deploy key\nfailure_reason: the cited page is from 2019\nextra:\n  intake_id: hb1\n---\n# Rotate\n"
	if _, err := st.PutStaged(ctx, "handbook", "hb1", []byte(cand)); err != nil {
		t.Fatal(err)
	}
	if err := st.Park(ctx, "hb1", "validators disagreed 2 times; last: the cited page is from 2019"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutStaged(ctx, "notes", "nt1", []byte("---\nid: intake/nt1\ntitle: Note\n---\n# N\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.Park(ctx, "nt1", "needs-human: which team owns this?"); err != nil {
		t.Fatal(err)
	}
	return reg, st, ms
}

func fakeForge(f forge.Client) ApplyOpts {
	return ApplyOpts{Forge: func(forge.Target, string) (forge.Client, error) { return f, nil }}
}

func byID(applied []Applied, id string) []Applied {
	var out []Applied
	for _, a := range applied {
		if a.IntakeID == id {
			out = append(out, a)
		}
	}
	return out
}

func librarianRun(t *testing.T, reg *collections.Registry, st *intake.Store) *Report {
	t.Helper()
	rep, err := Librarian(context.Background(), reg, st, LibrarianOpts{})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestEscalate_FilesOnceAndUnparksWhenResolved(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)
	f := &forgetest.Fake{}

	rep := librarianRun(t, reg, st)
	if rep.Count(FindingNeedsHuman) != 2 {
		t.Fatalf("needs_human = %d", rep.Count(FindingNeedsHuman))
	}
	var b bytes.Buffer
	rep.Write(&b)
	if !strings.Contains(b.String(), "handbook:hb1") || !strings.Contains(b.String(), "issue not yet filed") {
		t.Errorf("report:\n%s", b.String())
	}

	applied, err := ApplyWith(ctx, reg, st, rep, fakeForge(f))
	if err != nil {
		t.Fatal(err)
	}
	hb := byID(applied, "hb1")
	if len(hb) != 1 || hb[0].Action != "filed" || !strings.Contains(hb[0].Detail, "/example-org/handbook/issues/1") {
		t.Fatalf("hb1 applied = %+v", hb)
	}
	nt := byID(applied, "nt1")
	if len(nt) != 1 || nt[0].Detail != "no forge for notes; a human must look at parked/nt1.md" {
		t.Errorf("contract-less = %+v", nt)
	}

	issues := f.Issues()
	if len(issues) != 1 {
		t.Fatalf("issues = %+v", issues)
	}
	is := issues[0]
	if is.Repo != "example-org/handbook" || is.Title != "needs-human: Rotate the deploy key" {
		t.Errorf("issue = %q on %q", is.Title, is.Repo)
	}
	for _, want := range []string{
		"validators disagreed 2 times",            // the parked reason
		"intake_id: `hb1`",                        // the intake id
		"how do we rotate @everyone's deploy key", // the initial question
		"root → handbook",                         // the attempted path
		"the cited page is from 2019",             // the validator's failure reason
		"`resolved` label",                        // how to close the loop
	} {
		if !strings.Contains(is.Body, want) {
			t.Errorf("issue body lacks %q:\n%s", want, is.Body)
		}
	}
	// The question is agent text: fenced, so a mention does not ping.
	if !strings.Contains(is.Body, "```text\nhow do we rotate @everyone") {
		t.Errorf("question is not fenced:\n%s", is.Body)
	}
	if len(is.Labels) != 1 || is.Labels[0] != NeedsHumanLabel {
		t.Errorf("labels = %v", is.Labels)
	}

	// Second run: the reference is in the marker, nothing is filed.
	rep2 := librarianRun(t, reg, st)
	b.Reset()
	rep2.Write(&b)
	if !strings.Contains(b.String(), "issue https://forge.invalid/example-org/handbook/issues/1 open") {
		t.Errorf("second report:\n%s", b.String())
	}
	applied, err = ApplyWith(ctx, reg, st, rep2, fakeForge(f))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Issues()) != 1 || len(byID(applied, "hb1")) != 0 {
		t.Fatalf("second run filed again or said something about an open issue: %+v", applied)
	}

	// Closed without `resolved`: still parked, and said so.
	f.Close("example-org/handbook", 1, "wontfix")
	applied, _ = ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f))
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "skipped" || !strings.Contains(hb[0].Detail, "without the resolved label") {
		t.Errorf("closed without resolved = %+v", hb)
	}
	if parked, _ := st.Parked(ctx); parked["hb1"] == "" {
		t.Fatal("an unresolved close must leave the item parked")
	}

	// Closed with `resolved`: un-parked.
	f.Close("example-org/handbook", 1, ResolvedLabel)
	applied, err = ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f))
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "unparked" {
		t.Fatalf("resolved = %+v", hb)
	}
	parked, _ := st.Parked(ctx)
	if _, still := parked["hb1"]; still {
		t.Error("a resolved issue must un-park the item")
	}
	if _, other := parked["nt1"]; !other {
		t.Error("the contract-less item stays parked")
	}
	if len(f.Issues()) != 1 {
		t.Error("un-parking files nothing")
	}
}

func TestEscalate_MissingTokenSkipsWithReason(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "")
	reg, st := escalateFixture(t, escalateTokenEnv)
	f := &forgetest.Fake{}
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f))
	if err != nil {
		t.Fatalf("a missing token must not fail the run: %v", err)
	}
	hb := byID(applied, "hb1")
	if len(hb) != 1 || hb[0].Action != "skipped" || !strings.Contains(hb[0].Detail, "$"+escalateTokenEnv+" is not set") {
		t.Errorf("missing token = %+v", hb)
	}
	if len(f.Issues()) != 0 {
		t.Error("nothing may be filed without a token")
	}
	if items, _ := st.ParkedDetail(ctx); items[0].Issue != nil {
		t.Error("no reference without an issue")
	}
}

func TestEscalate_ContractWithoutTokenEnv(t *testing.T) {
	reg, st := escalateFixture(t, "")
	applied, err := ApplyWith(context.Background(), reg, st, librarianRun(t, reg, st), fakeForge(&forgetest.Fake{}))
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || !strings.Contains(hb[0].Detail, "names no token_env") {
		t.Errorf("no token_env = %+v", hb)
	}
}

func TestEscalate_ForgeErrorsAreReportedNotFatal(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)

	down := &forgetest.Fake{Err: errors.New("forge is down")}
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(down))
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "skipped" || !strings.Contains(hb[0].Detail, "forge is down") {
		t.Errorf("create failure = %+v", hb)
	}

	// A client that cannot be built.
	applied, _ = ApplyWith(ctx, reg, st, librarianRun(t, reg, st), ApplyOpts{Forge: func(forge.Target, string) (forge.Client, error) {
		return nil, errors.New("no client")
	}})
	if hb := byID(applied, "hb1"); len(hb) != 1 || !strings.Contains(hb[0].Detail, "no client") {
		t.Errorf("client failure = %+v", hb)
	}

	// The failed create left its claim: a run soon after files nothing
	// (the forge may have created the issue before failing), and a run
	// past the claim's staleness looks for it, finds none, and files.
	up := &forgetest.Fake{}
	soon := fakeForge(up)
	soon.Now = func() time.Time { return time.Now().Add(time.Minute) }
	applied, _ = ApplyWith(ctx, reg, st, librarianRun(t, reg, st), soon)
	if hb := byID(applied, "hb1"); len(hb) != 1 || !strings.Contains(hb[0].Detail, "already in flight") || len(up.Issues()) != 0 {
		t.Errorf("fresh claim = %+v", hb)
	}
	later := fakeForge(up)
	later.Now = func() time.Time { return time.Now().Add(filingStale + time.Minute) }
	applied, err = ApplyWith(ctx, reg, st, librarianRun(t, reg, st), later)
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "filed" || len(up.Issues()) != 1 {
		t.Fatalf("stale claim = %+v", hb)
	}

	// Filed, then the forge fails on the state read.
	up.Err = errors.New("forge is down")
	applied, _ = ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(up))
	if hb := byID(applied, "hb1"); len(hb) != 1 || !strings.Contains(hb[0].Detail, "read issue") {
		t.Errorf("state failure = %+v", hb)
	}
}

// SECURITY: an issue reference that does not match the forge the
// contract names now is never followed to un-park.
func TestEscalate_ForeignIssueReferenceIsNotFollowed(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)
	if err := st.SetParkedIssue(ctx, "hb1", intake.IssueRef{URL: "https://github.com/elsewhere/repo/issues/1", Host: "github", API: "https://api.github.com", Repo: "elsewhere/repo", Number: 1}); err != nil {
		t.Fatal(err)
	}
	f := &forgetest.Fake{}
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f))
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "skipped" || !strings.Contains(hb[0].Detail, "not on the forge the contract names") {
		t.Errorf("foreign reference = %+v", hb)
	}
	if parked, _ := st.Parked(ctx); parked["hb1"] == "" {
		t.Error("left parked")
	}
}

// SECURITY (review of #132): the same host kind and owner/repo on a
// different server is a different forge. A contract moved from
// github.com/example-org/handbook to a GitHub Enterprise server with the
// same slug must not follow (and un-park through) the old issue.
func TestEscalate_SameSlugOnAnotherServerIsNotFollowed(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)
	f := &forgetest.Fake{}
	if _, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f)); err != nil {
		t.Fatal(err)
	}
	f.Close("example-org/handbook", 1, ResolvedLabel)
	hb, _ := reg.Get("handbook")
	hb.Source.Update.Repo = "https://ghe.corp.example/example-org/handbook.git"
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f))
	if err != nil {
		t.Fatal(err)
	}
	if got := byID(applied, "hb1"); len(got) != 1 || got[0].Action != "skipped" || !strings.Contains(got[0].Detail, "not on the forge the contract names") {
		t.Errorf("moved contract = %+v", got)
	}
	if parked, _ := st.Parked(ctx); parked["hb1"] == "" {
		t.Error("an issue on the old server must not un-park the item")
	}
}

// failRecordStore refuses the write that records an issue reference.
type failRecordStore struct{ *memory.LocalStore }

func (s failRecordStore) Put(ctx context.Context, key string, body []byte, pre memory.Precondition) (memory.Version, error) {
	if strings.HasPrefix(key, "parked/") && strings.Contains(string(body), "\nurl: ") {
		return "", errors.New("store is read-only now")
	}
	return s.LocalStore.Put(ctx, key, body, pre)
}

// Filed but not recorded: the run fails and names the issue, so the
// operator is not left with a silent orphan; the claim stays, and the
// next run adopts the issue instead of filing it again.
func TestEscalate_FiledButNotRecordedFailsTheRunAndNamesTheIssue(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st, ls := escalateFixtureOn(t, escalateTokenEnv)
	bad := intake.New(failRecordStore{ls})
	f := &forgetest.Fake{}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	opts := fakeForge(f)
	opts.Now = func() time.Time { return now }
	_, err := ApplyWith(ctx, reg, bad, librarianRun(t, reg, bad), opts)
	if err == nil || !strings.Contains(err.Error(), "https://forge.invalid/example-org/handbook/issues/1") || !strings.Contains(err.Error(), "could not record it in parked/hb1.md") {
		t.Fatalf("want a failed run naming the issue, got %v", err)
	}
	// The store recovers; a run past the claim's staleness adopts issue 1.
	now = now.Add(filingStale + time.Minute)
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), opts)
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "adopted" || !strings.HasSuffix(hb[0].Detail, "/issues/1") {
		t.Fatalf("after recovery = %+v", hb)
	}
	if len(f.Issues()) != 1 {
		t.Errorf("filed %d issues, want 1", len(f.Issues()))
	}
}

// timeoutClient creates the issue on the forge and then reports a
// failure, as a client timeout after a server-side create does.
type timeoutClient struct{ *forgetest.Fake }

func (c timeoutClient) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (string, int, error) {
	if _, _, err := c.Fake.CreateIssue(ctx, repo, title, body, labels); err != nil {
		return "", 0, err
	}
	return "", 0, errors.New("POST /repos/example-org/handbook/issues: context deadline exceeded")
}

// Review of #132: a create that times out after the forge made the
// issue must not lead to a second issue. The claim left in the marker
// makes the next run look for the issue first and adopt it; a run
// while the claim is fresh files nothing.
func TestEscalate_AdoptsTheIssueAnInterruptedRunFiled(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)
	f := &forgetest.Fake{}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	opts := fakeForge(timeoutClient{f})
	opts.Now = func() time.Time { return now }

	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), opts)
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "skipped" || !strings.Contains(hb[0].Detail, "deadline exceeded") {
		t.Fatalf("timed-out create = %+v", hb)
	}
	rep := librarianRun(t, reg, st)
	var b bytes.Buffer
	rep.Write(&b)
	if !strings.Contains(b.String(), "issue filing began 2026-10-02T12:00:00Z, not recorded") {
		t.Errorf("report:\n%s", b.String())
	}

	// Another run a minute later: the claim is fresh, nothing is filed.
	now = now.Add(time.Minute)
	opts.Forge = fakeForge(f).Forge
	applied, err = ApplyWith(ctx, reg, st, rep, opts)
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "skipped" || !strings.Contains(hb[0].Detail, "already in flight") {
		t.Fatalf("fresh claim = %+v", hb)
	}

	// Past the claim's staleness: the issue is found by its marker line
	// and adopted, not filed again.
	now = now.Add(filingStale)
	applied, err = ApplyWith(ctx, reg, st, librarianRun(t, reg, st), opts)
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "adopted" || hb[0].Detail != "issue https://forge.invalid/example-org/handbook/issues/1" {
		t.Fatalf("adopt = %+v", hb)
	}
	if len(f.Issues()) != 1 {
		t.Errorf("filed %d issues, want 1", len(f.Issues()))
	}
	items, _ := st.ParkedDetail(ctx)
	if items[0].Issue == nil || items[0].Issue.Number != 1 || items[0].Issue.API != "https://api.github.com" {
		t.Errorf("recorded = %+v", items[0].Issue)
	}

	// A stale claim with nothing to adopt files normally.
	reg2, st2 := escalateFixture(t, escalateTokenEnv)
	f2 := &forgetest.Fake{}
	now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if _, err := st2.BeginFiling(ctx, "hb1", now.Add(-time.Hour), filingStale); err != nil {
		t.Fatal(err)
	}
	opts2 := fakeForge(f2)
	opts2.Now = func() time.Time { return now }
	applied, _ = ApplyWith(ctx, reg2, st2, librarianRun(t, reg2, st2), opts2)
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "filed" || len(f2.Issues()) != 1 {
		t.Fatalf("stale claim, nothing to adopt = %+v", hb)
	}
}

// raceClient files, and before the run records the issue another run
// records its own, as two concurrent --apply runs would when one took
// over the other's stale claim.
type raceClient struct {
	*forgetest.Fake
	st *intake.Store
}

func (c raceClient) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (string, int, error) {
	first, n, err := c.Fake.CreateIssue(ctx, repo, title, body, labels)
	if err != nil {
		return "", 0, err
	}
	if err := c.st.SetParkedIssue(ctx, "hb1", intake.IssueRef{URL: first, Host: "github", API: "https://api.github.com", Repo: repo, Number: n}); err != nil {
		return "", 0, err
	}
	return c.Fake.CreateIssue(ctx, repo, title, body, labels)
}

// Review of #132: when another run recorded its issue first, the issue
// this run filed is reported as a duplicate by URL, not as "filed".
func TestEscalate_ConcurrentFilingReportsTheDuplicate(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)
	f := &forgetest.Fake{}
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(raceClient{f, st}))
	if err != nil {
		t.Fatal(err)
	}
	hb := byID(applied, "hb1")
	want := "issue https://forge.invalid/example-org/handbook/issues/2 duplicates https://forge.invalid/example-org/handbook/issues/1, which another run recorded first; close https://forge.invalid/example-org/handbook/issues/2 by hand"
	if len(hb) != 1 || hb[0].Action != "duplicate" || hb[0].Detail != want {
		t.Fatalf("race = %+v", hb)
	}
	items, _ := st.ParkedDetail(ctx)
	if items[0].Issue == nil || items[0].Issue.Number != 1 {
		t.Errorf("the first filing wins: %+v", items[0].Issue)
	}

	// A report taken before the other run recorded its issue: the claim
	// finds the reference and files nothing.
	reg2, st2 := escalateFixture(t, escalateTokenEnv)
	stale := librarianRun(t, reg2, st2)
	if err := st2.SetParkedIssue(ctx, "hb1", intake.IssueRef{URL: "https://forge.invalid/x/1", Host: "github", API: "https://api.github.com", Repo: "example-org/handbook", Number: 1}); err != nil {
		t.Fatal(err)
	}
	f2 := &forgetest.Fake{}
	applied, _ = ApplyWith(ctx, reg2, st2, stale, fakeForge(f2))
	if hb := byID(applied, "hb1"); len(hb) != 1 || !strings.Contains(hb[0].Detail, "was recorded by another run") || len(f2.Issues()) != 0 {
		t.Fatalf("stale report = %+v, issues %d", hb, len(f2.Issues()))
	}
}

// An issue section a human mangled is reported, never filed over.
func TestEscalate_UnreadableIssueSectionIsReported(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st, ls := escalateFixtureOn(t, escalateTokenEnv)
	recs, _ := ls.Load(ctx)
	for _, r := range recs {
		if r.Key == "parked/hb1.md" {
			body := string(r.Body) + "\n## forge issue\n\nurl: https://forge.invalid/x/1\nnumber: one\n"
			if _, err := ls.Put(ctx, r.Key, []byte(body), memory.UpdateFrom(r.Version)); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := &forgetest.Fake{}
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f))
	if err != nil {
		t.Fatal(err)
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "skipped" || !strings.Contains(hb[0].Detail, "issue section is unreadable") {
		t.Fatalf("unreadable = %+v", hb)
	}
	if len(f.Issues()) != 0 {
		t.Error("nothing is filed over an unreadable section")
	}
}

// SECURITY (review of #132): the attempted path and the collection
// derived from it are agent-written. A backtick or a blank line in them
// must not end the code span and put a live mention or link into the
// issue.
func TestIssueBody_AgentTextCannotLeaveItsCodeSpan(t *testing.T) {
	p := ParkedEntry{IntakeID: "abc", Collection: "hand`book\n\n@example-org/owners", Reason: "r",
		Attempted: []string{"root", "x`\n\n@example-org/security please look [here](https://evil.example)\n\n`y", "handbook"}}
	body := issueBody(p, "parked/abc.md")
	for _, want := range []string{
		"- attempted path: `root → x' @example-org/security please look [here](https://evil.example) 'y → handbook`\n",
		"- target collection: `hand'book @example-org/owners`\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	// Every mention sits inside a code span on its own list line.
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "@example-org") && !strings.HasPrefix(line, "- ") {
			t.Errorf("a mention escaped onto its own line: %q", line)
		}
	}
	if !strings.HasPrefix(body, "<!-- meerkat:intake-id abc -->\n") {
		t.Errorf("the marker line comes first:\n%s", body)
	}
	if got := issueMarker("a-->b <x>"); got != "<!-- meerkat:intake-id a--_b__x_ -->" {
		t.Errorf("marker = %q", got)
	}
}

func TestEscalate_UnsupportedHostAndDefaultClient(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)
	hb, _ := reg.Get("handbook")
	hb.Source.Update.Host = "gitlab"
	applied, err := Apply(ctx, reg, st, librarianRun(t, reg, st))
	if err != nil {
		t.Fatal(err)
	}
	if got := byID(applied, "hb1"); len(got) != 1 || !strings.Contains(got[0].Detail, "not supported yet") {
		t.Errorf("gitlab = %+v", got)
	}
}

func TestIssueTitleAndFence(t *testing.T) {
	if got := issueTitle(ParkedEntry{IntakeID: "x1"}); got != "needs-human: x1" {
		t.Errorf("title without a candidate = %q", got)
	}
	long := issueTitle(ParkedEntry{Title: strings.Repeat("word ", 60)})
	if n := len([]rune(long)); n != maxIssueTitle || !strings.HasSuffix(long, "…") {
		t.Errorf("long title = %d runes %q", n, long)
	}
	if got := fenced("a ``` b"); !strings.HasPrefix(got, "````text\n") {
		t.Errorf("fence must outgrow the text's backticks: %q", got)
	}
}

func TestParkedEntries_FallsBackToTheRawItem(t *testing.T) {
	ctx := context.Background()
	ms, _ := memory.OpenLocal(filepath.Join(t.TempDir(), "intake"))
	st := intake.New(ms)
	raw := []byte("---\n{\"id\":\"r1\",\"question\":\"q\",\"attempted\":[\"root\",\"platform/flux\"],\"target_kb\":\"flux\"}\n---\nbody\n")
	if _, err := st.PutRaw(ctx, "ns", time.Now(), "r1", raw); err != nil {
		t.Fatal(err)
	}
	if err := st.Park(ctx, "r1", "needs-human: x"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRaw(ctx, "ns", time.Now(), "bad", []byte("no frontmatter")); err != nil {
		t.Fatal(err)
	}
	if err := st.Park(ctx, "bad", "needs-human: y"); err != nil {
		t.Fatal(err)
	}
	got, err := parkedEntries(ctx, st, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("entries = %+v %v", got, err)
	}
	if got[0].IntakeID != "bad" || got[0].Collection != "unrouted" || got[0].Question != "" {
		t.Errorf("malformed raw = %+v", got[0])
	}
	if got[1].Collection != "flux" || got[1].Question != "q" {
		t.Errorf("raw fallback = %+v", got[1])
	}
}
