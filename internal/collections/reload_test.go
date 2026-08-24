package collections

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// reload_test.go covers runtime reconciliation at the layer this package
// owns: the probe/resolve/rebuild/swap cycle, what a failure leaves
// serving, and that a swap is invisible to a concurrent request.
//
// The GCS mechanics one layer down — conditional reads, generation
// preconditions, extraction hardening, the size and object-count caps —
// are covered against internal/contentsource's own in-memory fake. Here
// the two calls into that package are swapped for a scripted seam (see
// probeVersion / resolveContent), so a test can produce a failed
// conditional read, a malformed generation or a mid-rebuild write on
// demand.

// --- a scripted content source -----------------------------------------

// fakeSource stands in for a GCS bucket: a sequence of generations, each
// one a directory of files, plus the failures a real bucket produces.
type fakeSource struct {
	t *testing.T

	mu sync.Mutex
	// version is what a probe currently answers.
	version string
	// dirs maps a version to the resolved local directory for it.
	dirs map[string]string
	// probeErr / resolveErr, when set, fail the corresponding step.
	probeErr   error
	resolveErr error
	// probes / resolves count the calls, to prove an unchanged probe
	// downloads nothing.
	probes   int
	resolves int
	// resolveGate, when non-nil, is received from inside resolve — the
	// hook that holds a rebuild open while something else happens.
	resolveGate chan struct{}
}

func newFakeSource(t *testing.T) *fakeSource {
	t.Helper()
	f := &fakeSource{t: t, dirs: map[string]string{}}
	origProbe, origResolve := probeVersion, resolveContent
	probeVersion = f.probe
	resolveContent = f.resolve
	t.Cleanup(func() { probeVersion, resolveContent = origProbe, origResolve })
	return f
}

// publish writes a new generation and makes it the live one.
func (f *fakeSource) publish(version string, files map[string]string) {
	f.t.Helper()
	dir := f.t.TempDir()
	writeTree(f.t, dir, files)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[version] = dir
	f.version = version
}

// publishBroken makes version live but unresolvable — a generation that
// lists fine and then fails the conditional read, or a bundle that is
// not a valid archive.
func (f *fakeSource) publishBroken(version string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = version
	f.resolveErr = err
}

func (f *fakeSource) failProbe(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeErr = err
}

func (f *fakeSource) probe(context.Context, contentsource.Source) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	if f.probeErr != nil {
		return "", f.probeErr
	}
	return f.version, nil
}

func (f *fakeSource) resolve(context.Context, contentsource.Source) (string, string, error) {
	f.mu.Lock()
	f.resolves++
	gate, err, version := f.resolveGate, f.resolveErr, f.version
	dir := f.dirs[version]
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return "", "", err
	}
	return dir, version, nil
}

func (f *fakeSource) counts() (probes, resolves int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes, f.resolves
}

// gcsSource is the content-source.yaml entry a refreshable collection
// has. It never reaches a real bucket — the seam above intercepts both
// calls — but it is a genuinely valid one, so validation and
// Refreshable() are exercised for real.
func gcsSource(policy string) contentsource.Source {
	src := contentsource.Source{
		Type:   contentsource.TypeGCS,
		Bucket: "example-kb",
		Prefix: "handbook/live/",
		Layout: contentsource.MergeLayout(contentsource.Layout{}),
		Refresh: &refresh.Spec{
			Interval:      refresh.Duration(time.Minute),
			FailurePolicy: policy,
		},
	}
	return src
}

// refreshableCollection mounts one collection over the fake source's
// current generation, exactly as Open would.
func refreshableCollection(t *testing.T, f *fakeSource, policy string) *Collection {
	t.Helper()
	src := gcsSource(policy)
	if err := src.Validate(); err != nil {
		t.Fatalf("the test's own source is invalid: %v", err)
	}
	f.mu.Lock()
	dir, version := f.dirs[f.version], f.version
	f.mu.Unlock()

	reg, err := Open(context.Background(), []contentsource.ResolvedCollection{
		{Name: "handbook", Dir: dir, Source: src, Provenance: "gcs://example-kb/handbook/live/*@" + version, Version: version},
		// A second collection so Open gives each its own filesystem — the
		// multi-collection shape a hosted deployment is in.
		{Name: "static", Dir: t.TempDir(), Source: contentsource.Source{Type: contentsource.TypeLocal, Layout: contentsource.MergeLayout(contentsource.Layout{})}, Provenance: "disk:x"},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	c, err := reg.Get("handbook")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wikiPage(id, body string) string {
	return "---\nid: " + id + "\ntitle: " + id + "\n---\n\n" + body + "\n"
}

// searchIDs runs a query and returns the matching page IDs.
func searchIDs(t *testing.T, c *Collection, query string) []string {
	t.Helper()
	results, err := c.searchAs(context.Background(), kb.Unfiltered(), query, 20)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Page.ID)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- content reconciliation --------------------------------------------

// TestReloadContent_NewGenerationBecomesSearchableWithoutARestart is the
// headline acceptance criterion.
func TestReloadContent_NewGenerationBecomesSearchableWithoutARestart(t *testing.T) {
	ctx := context.Background()
	f := newFakeSource(t)
	f.publish("gen-1", map[string]string{"wiki/onboarding.md": wikiPage("onboarding", "the axolotl handbook")})
	c := refreshableCollection(t, f, "")

	if got := searchIDs(t, c, "axolotl"); len(got) != 1 || got[0] != "onboarding" {
		t.Fatalf("before the refresh, search = %v", got)
	}
	if got := searchIDs(t, c, "pangolin"); len(got) != 0 {
		t.Fatalf("the new generation's content is already visible: %v", got)
	}

	f.publish("gen-2", map[string]string{
		"wiki/onboarding.md": wikiPage("onboarding", "the axolotl handbook"),
		"wiki/runbook.md":    wikiPage("runbook", "the pangolin runbook"),
	})
	out, err := c.ReloadContent(ctx)
	if err != nil {
		t.Fatalf("ReloadContent: %v", err)
	}
	if !out.Changed || out.Version != "gen-2" {
		t.Fatalf("outcome = %+v, want a change to gen-2", out)
	}

	if got := searchIDs(t, c, "pangolin"); len(got) != 1 || got[0] != "runbook" {
		t.Errorf("after the refresh, search for the new page = %v", got)
	}
	pages, err := c.Pages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Errorf("Pages() = %d entries, want the new generation's 2", len(pages))
	}
	if _, err := c.Load("runbook"); err != nil {
		t.Errorf("show of a page from the new generation: %v", err)
	}
	// Provenance follows the bytes, so "what is this replica serving?"
	// answers the new generation rather than the one it booted on.
	if !strings.Contains(c.Provenance(), "gen-2") {
		t.Errorf("Provenance() = %q, want the new generation", c.Provenance())
	}
}

// TestReloadContent_AddAndDeleteLandAsOneSnapshot: a generation that
// both adds and removes pages must be applied whole. A caller must never
// see the added page without the deletion, or vice versa.
func TestReloadContent_AddAndDeleteLandAsOneSnapshot(t *testing.T) {
	f := newFakeSource(t)
	f.publish("gen-1", map[string]string{
		"wiki/keep.md":    wikiPage("keep", "kept across the change"),
		"wiki/removed.md": wikiPage("removed", "about to disappear"),
	})
	c := refreshableCollection(t, f, "")
	if _, err := c.Index(); err != nil {
		t.Fatal(err)
	}

	f.publish("gen-2", map[string]string{
		"wiki/keep.md":  wikiPage("keep", "kept across the change"),
		"wiki/added.md": wikiPage("added", "brand new"),
	})
	if _, err := c.ReloadContent(context.Background()); err != nil {
		t.Fatalf("ReloadContent: %v", err)
	}

	pages, err := c.Pages()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(pages))
	for _, p := range pages {
		ids = append(ids, p.ID)
	}
	if len(ids) != 2 || !contains(ids, "keep") || !contains(ids, "added") {
		t.Fatalf("pages = %v, want exactly keep and added", ids)
	}
	if contains(searchIDs(t, c, "disappear"), "removed") {
		t.Error("the deleted page is still in the search index — the swap was not whole")
	}
	if !contains(searchIDs(t, c, "brand"), "added") {
		t.Error("the added page is missing from the search index")
	}
	if _, err := c.Load("removed"); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("show of a deleted page = %v, want ErrNotFound", err)
	}
}

// TestReloadContent_UnchangedVersionDoesNoWork is the reason polling is
// affordable: the common case is one metadata call and nothing else.
func TestReloadContent_UnchangedVersionDoesNoWork(t *testing.T) {
	f := newFakeSource(t)
	f.publish("gen-1", map[string]string{"wiki/a.md": wikiPage("a", "body")})
	c := refreshableCollection(t, f, "")
	if _, err := c.Index(); err != nil {
		t.Fatal(err)
	}
	before := c.builtIndex()

	out, err := c.ReloadContent(context.Background())
	if err != nil {
		t.Fatalf("ReloadContent: %v", err)
	}
	if out.Changed {
		t.Error("an unchanged source reported a change")
	}
	if _, resolves := f.counts(); resolves != 0 {
		t.Errorf("resolves = %d, want 0 — an unchanged probe must not download or reindex", resolves)
	}
	if c.builtIndex() != before {
		t.Error("an unchanged probe rebuilt the index")
	}
	statuses := c.ReloadStatuses()
	if len(statuses) != 1 || statuses[0].LastSuccess.IsZero() {
		t.Errorf("status = %+v, want an unchanged probe to record a successful cycle", statuses)
	}
	if statuses[0].Version != "gen-1" {
		t.Errorf("status version = %q, want the version being served", statuses[0].Version)
	}
}

// TestReloadContent_FailedResolveKeepsServingTheLastGood is the
// serve-last-good contract: a malformed or unauthorized new generation
// changes nothing about what is served, and says so.
func TestReloadContent_FailedResolveKeepsServingTheLastGood(t *testing.T) {
	f := newFakeSource(t)
	f.publish("gen-1", map[string]string{"wiki/a.md": wikiPage("a", "the axolotl handbook")})
	c := refreshableCollection(t, f, "")
	if _, err := c.Index(); err != nil {
		t.Fatal(err)
	}
	goodIndex := c.builtIndex()

	// A new generation that lists fine and then fails the conditional
	// read — the shape of an object replaced mid-fetch, or a 403.
	f.publishBroken("gen-2", errors.New("generation 2 of \"kb.tar.gz\" does not exist (precondition failed)"))
	_, err := c.ReloadContent(context.Background())
	if err == nil {
		t.Fatal("expected the failed resolve to be reported")
	}
	if !strings.Contains(err.Error(), "handbook") {
		t.Errorf("error = %v, want it to name the collection", err)
	}

	// Still serving, still the old bytes, same index object.
	if got := searchIDs(t, c, "axolotl"); len(got) != 1 || got[0] != "a" {
		t.Errorf("search after a failed refresh = %v, want the last known-good content", got)
	}
	if c.builtIndex() != goodIndex {
		t.Error("a failed refresh replaced the index")
	}
	if c.currentVersion() != "gen-1" {
		t.Errorf("version = %q, want the last known-good gen-1", c.currentVersion())
	}
	if !strings.Contains(c.Provenance(), "gen-1") {
		t.Errorf("Provenance() = %q, want the last known-good generation", c.Provenance())
	}

	// ... and it is reported as degraded, while staying READY: it is
	// answering queries correctly, just not with current content.
	h := c.health()
	if !h.Degraded {
		t.Error("a failed refresh did not mark the collection degraded")
	}
	if !h.Ready {
		t.Error("serve-last-good must not take a serving collection out of rotation")
	}
	if !strings.Contains(h.StaleReason, "content refresh failed") {
		t.Errorf("StaleReason = %q", h.StaleReason)
	}

	// A later good generation clears it.
	f.mu.Lock()
	f.resolveErr = nil
	f.mu.Unlock()
	f.publish("gen-3", map[string]string{"wiki/a.md": wikiPage("a", "the pangolin handbook")})
	if _, err := c.ReloadContent(context.Background()); err != nil {
		t.Fatalf("ReloadContent after recovery: %v", err)
	}
	if h := c.health(); h.Degraded {
		t.Errorf("a successful refresh did not clear the degraded state: %+v", h)
	}
	if !contains(searchIDs(t, c, "pangolin"), "a") {
		t.Error("the recovered generation is not being served")
	}
}

// TestReloadContent_FailedProbeIsDegradedToo: not being able to ASK
// whether content changed is as much a staleness problem as not being
// able to fetch it.
func TestReloadContent_FailedProbeIsDegraded(t *testing.T) {
	f := newFakeSource(t)
	f.publish("gen-1", map[string]string{"wiki/a.md": wikiPage("a", "body")})
	c := refreshableCollection(t, f, "")

	f.failProbe(errors.New("could not find default credentials"))
	if _, err := c.ReloadContent(context.Background()); err == nil {
		t.Fatal("expected the probe failure to be reported")
	}
	if _, resolves := f.counts(); resolves != 0 {
		t.Error("a failed probe must not go on to resolve anything")
	}
	h := c.health()
	if !h.Degraded || !h.Ready {
		t.Errorf("health = %+v, want degraded but still serving", h)
	}
}

// TestReloadContent_UnreadyPolicyFailsReadiness is the other trade: a
// collection whose whole value is being current asks to be drained
// rather than serve stale content.
func TestReloadContent_UnreadyPolicyFailsReadiness(t *testing.T) {
	f := newFakeSource(t)
	f.publish("gen-1", map[string]string{"wiki/a.md": wikiPage("a", "body")})
	c := refreshableCollection(t, f, refresh.PolicyUnready)

	f.publishBroken("gen-2", errors.New("403"))
	if _, err := c.ReloadContent(context.Background()); err == nil {
		t.Fatal("expected the failed resolve to be reported")
	}
	h := c.health()
	if !h.Degraded {
		t.Error("the collection should be degraded")
	}
	if h.Ready {
		t.Error("failure_policy: unready must fail readiness")
	}
	// It is still SERVING, though: draining is not refusing.
	if got := searchIDs(t, c, "body"); len(got) != 1 {
		t.Errorf("an unready collection stopped answering queries: %v", got)
	}
}

// TestReloadContent_RefusesToOverlapItself proves the per-collection
// reload slot: a second cycle arriving while one is mid-rebuild is
// refused with ErrBusy rather than staging a second swap.
func TestReloadContent_RefusesToOverlapItself(t *testing.T) {
	f := newFakeSource(t)
	f.publish("gen-1", map[string]string{"wiki/a.md": wikiPage("a", "body")})
	c := refreshableCollection(t, f, "")

	gate := make(chan struct{})
	f.mu.Lock()
	f.resolveGate = gate
	f.mu.Unlock()
	f.publish("gen-2", map[string]string{"wiki/a.md": wikiPage("a", "new body")})

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := c.ReloadContent(context.Background())
		done <- err
	}()
	<-started
	// Wait until the first cycle is actually inside resolve.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, resolves := f.counts(); resolves > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first cycle never reached resolve")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := c.ReloadContent(context.Background()); !errors.Is(err, refresh.ErrBusy) {
		t.Errorf("the overlapping cycle = %v, want ErrBusy", err)
	}
	// A memory reconcile for the SAME collection is refused too: both
	// stage against one journal.
	if _, err := c.ReloadMemory(context.Background()); !errors.Is(err, refresh.ErrBusy) {
		t.Errorf("an overlapping memory cycle = %v, want ErrBusy", err)
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("the first cycle: %v", err)
	}
	if _, resolves := f.counts(); resolves != 1 {
		t.Errorf("resolves = %d, want exactly 1 — the overlap must not have fetched", resolves)
	}
}

// TestReloadContent_RefusesANonRefreshableSource: the runtime predicate
// agrees with the config-time refusal, so a pinned source cannot be
// reconciled even by a direct call.
func TestReloadContent_RefusesANonRefreshableSource(t *testing.T) {
	c := FromPages("notes", []kb.Page{page("a", "A", "body")})
	if _, err := c.ReloadContent(context.Background()); err == nil {
		t.Fatal("a collection with no refresh block should refuse to reconcile")
	}

	pinned := gcsSource("")
	pinned.Prefix, pinned.Object, pinned.Generation = "", "kb.tar.gz", 42
	c.Source = pinned
	if _, err := c.ReloadContent(context.Background()); err == nil {
		t.Fatal("a pinned-generation source should refuse to reconcile")
	}
}

// --- memory reconciliation ---------------------------------------------

// TestReloadMemory_TwoRegistriesSharingOneBucketConverge is the replica
// acceptance criterion, and the one that also has to hold the #27 line:
// a personal memory written through replica A must become visible to its
// OWNER on replica B, and to nobody else.
func TestReloadMemory_TwoRegistriesSharingOneBucketConverge(t *testing.T) {
	ctx := context.Background()
	bucket := newFakeMemoryBucket()

	newReplica := func() (*Registry, *Collection) {
		t.Helper()
		c := FromPages("notes", []kb.Page{page("handbook/onboarding", "Onboarding", "the axolotl handbook")})
		c.Source = memoryRefreshSource()
		c.status.configure(c.Source)
		if err := c.AttachMemory(ctx, bucket.store()); err != nil {
			t.Fatalf("AttachMemory: %v", err)
		}
		reg, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reg.Close() })
		return reg, c
	}

	regA, colA := newReplica()
	regB, colB := newReplica()
	// Build both indexes up front, so what follows is a real reload of a
	// live index rather than a first build that happened to include the
	// memory.
	for _, c := range []*Collection{colA, colB} {
		if _, err := c.Index(); err != nil {
			t.Fatal(err)
		}
	}

	// Alice saves a personal memory through replica A.
	save(t, colA, personalKey(aliceNS, "salary"), "Salary", "the axolotl detail")
	id := personalID(aliceNS, "salary")

	// Replica B knows nothing about it yet.
	if _, err := regB.ViewedBy(kb.AsOwner(aliceNS)).Show("", id); !errors.Is(err, kb.ErrNotFound) {
		t.Fatalf("replica B saw the memory before reconciling: %v", err)
	}

	out, err := colB.ReloadMemory(ctx)
	if err != nil {
		t.Fatalf("replica B ReloadMemory: %v", err)
	}
	if !out.Changed {
		t.Fatal("replica B did not notice replica A's write")
	}

	// Now it converges — for the OWNER.
	if _, err := regB.ViewedBy(kb.AsOwner(aliceNS)).Show("", id); err != nil {
		t.Errorf("after reconciling, alice cannot read her own memory on replica B: %v", err)
	}
	hits, err := regB.ViewedBy(kb.AsOwner(aliceNS)).Search(ctx, "", "axolotl", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Errorf("alice's search on replica B = %+v, want the public page and her memory", hits)
	}

	// ... and NOT for anybody else. This is the #27 invariant surviving a
	// reload: the owner is derived from the store key, so a document
	// re-read on another process is still private to the same principal.
	if _, err := regB.ViewedBy(kb.AsOwner(bobNS)).Show("", id); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("after reconciling, alice's memory is readable by bob on replica B: %v", err)
	}
	bobHits, err := regB.ViewedBy(kb.AsOwner(bobNS)).Search(ctx, "", "axolotl", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobHits) != 1 || bobHits[0].Page.ID != "handbook/onboarding" {
		t.Errorf("bob's search on replica B = %+v, want only the public page", bobHits)
	}
	bobPages, err := regB.ViewedBy(kb.AsOwner(bobNS)).Pages("")
	if err != nil {
		t.Fatal(err)
	}
	if hasID(bobPages, id) {
		t.Errorf("bob can list alice's memory after a reload: %v", ids(bobPages))
	}

	// Convergence is symmetric: B writes, A reconciles, A sees it.
	save(t, colB, "team/runbook.md", "Runbook", "the pangolin runbook")
	if _, err := colA.ReloadMemory(ctx); err != nil {
		t.Fatalf("replica A ReloadMemory: %v", err)
	}
	if _, err := regA.Show("", "memory/team/runbook"); err != nil {
		t.Errorf("replica A did not converge on replica B's write: %v", err)
	}

	// A second cycle with nothing new does no work.
	before := bucket.loads()
	out, err = colA.ReloadMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed {
		t.Error("an unchanged store reported a change")
	}
	if bucket.loads() != before {
		t.Error("an unchanged fingerprint still re-read every document")
	}
}

// TestReloadMemory_KeepsAWriteThatLandedDuringTheRebuild is the
// lost-write race the staging journal exists for. A memory saved while
// the replacement index is being built must survive the swap — it was
// durably stored and the caller was told so.
func TestReloadMemory_KeepsAWriteThatLandedDuringTheRebuild(t *testing.T) {
	ctx := context.Background()
	bucket := newFakeMemoryBucket()
	c := FromPages("notes", []kb.Page{page("handbook/onboarding", "Onboarding", "shared body")})
	c.Source = memoryRefreshSource()
	c.status.configure(c.Source)
	if err := c.AttachMemory(ctx, bucket.store()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Index(); err != nil {
		t.Fatal(err)
	}

	// Something another replica wrote, so this cycle has work to do.
	other := bucket.store()
	if _, err := other.Put(ctx, "team/remote.md",
		memoryDoc(t, "Remote", "written by the other replica"), memory.CreateOnly()); err != nil {
		t.Fatal(err)
	}

	// Hold the rebuild open at the store Load, then save locally.
	gate := make(chan struct{})
	bucket.gateLoad(gate)
	done := make(chan error, 1)
	go func() {
		_, err := c.ReloadMemory(ctx)
		done <- err
	}()
	bucket.waitForLoad(t)
	save(t, c, "team/local.md", "Local", "written during the rebuild")
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("ReloadMemory: %v", err)
	}

	// Both are present, and present on EVERY surface: the overlay backs
	// show and list, the rebuilt index backs search. A save that landed
	// in one but not the other would be findable by search and invisible
	// to show, which is exactly the split the shared lock exists to
	// prevent.
	pages, err := c.Pages()
	if err != nil {
		t.Fatal(err)
	}
	listed := make([]string, 0, len(pages))
	for _, p := range pages {
		listed = append(listed, p.ID)
	}
	for _, id := range []string{"memory/team/remote", "memory/team/local"} {
		if _, err := c.Load(id); err != nil {
			t.Errorf("show %s after the reload: %v", id, err)
		}
		if !contains(listed, id) {
			t.Errorf("list after the reload = %v, want it to contain %s", listed, id)
		}
	}
	if !contains(searchIDs(t, c, "rebuild"), "memory/team/local") {
		t.Error("the memory saved during the rebuild is missing from the rebuilt index — the swap lost a write")
	}
	if !contains(searchIDs(t, c, "replica"), "memory/team/remote") {
		t.Error("the other replica's memory is missing from the rebuilt index")
	}
}

// TestReloadMemory_FailedLoadKeepsTheOverlay: a store that goes away
// must not empty the overlay. Losing every memory because a listing
// failed would be the worst possible reading of "reconcile".
func TestReloadMemory_FailedLoadKeepsTheOverlay(t *testing.T) {
	ctx := context.Background()
	bucket := newFakeMemoryBucket()
	c := FromPages("notes", []kb.Page{page("handbook/onboarding", "Onboarding", "shared body")})
	c.Source = memoryRefreshSource()
	c.status.configure(c.Source)
	if err := c.AttachMemory(ctx, bucket.store()); err != nil {
		t.Fatal(err)
	}
	save(t, c, "team/keep.md", "Keep", "must survive a failed reconcile")

	bucket.failWith(errors.New("the bucket is unreachable"))
	if _, err := c.ReloadMemory(ctx); err == nil {
		t.Fatal("expected the failed probe to be reported")
	}
	if _, err := c.Load("memory/team/keep"); err != nil {
		t.Errorf("a failed reconcile dropped an existing memory: %v", err)
	}
	if !contains(searchIDs(t, c, "survive"), "memory/team/keep") {
		t.Error("a failed reconcile emptied the index")
	}
	h := c.health()
	if !h.Degraded || !h.Ready {
		t.Errorf("health = %+v, want degraded but still serving", h)
	}
}

// TestReloadMemory_SkipsAnUnparseableDocument: one bad object must not
// stop a fleet from converging on the rest.
//
// The only way memory.Page can fail is a body over internal/kb's page
// cap, so that is what this writes. The GCS backend's own Load already
// refuses anything over its (much smaller) document cap before it gets
// here, so this is defence in depth rather than a routine path — the
// same defence AttachMemory has at mount time, which is precisely why it
// has to be here too: a reload that failed outright on one bad object
// would stop a replica converging until somebody noticed.
func TestReloadMemory_SkipsAnUnparseableDocument(t *testing.T) {
	ctx := context.Background()
	bucket := newFakeMemoryBucket()
	c := FromPages("notes", nil)
	c.Source = memoryRefreshSource()
	c.status.configure(c.Source)
	if err := c.AttachMemory(ctx, bucket.store()); err != nil {
		t.Fatal(err)
	}
	other := bucket.store()
	if _, err := other.Put(ctx, "team/good.md", memoryDoc(t, "Good", "parses fine"), memory.CreateOnly()); err != nil {
		t.Fatal(err)
	}
	oversized := append([]byte("---\ntitle: Huge\n---\n\n"), bytes.Repeat([]byte("x"), 9<<20)...)
	if _, err := other.Put(ctx, "team/huge.md", oversized, memory.CreateOnly()); err != nil {
		t.Fatal(err)
	}

	stderr := captureCollectionStderr(t, func() {
		if _, err := c.ReloadMemory(ctx); err != nil {
			t.Fatalf("ReloadMemory: %v", err)
		}
	})
	if !strings.Contains(stderr, "skipping memory") {
		t.Errorf("an unparseable document was skipped silently: %q", stderr)
	}
	if _, err := c.Load("memory/team/good"); err != nil {
		t.Errorf("the well-formed document did not load: %v", err)
	}
	if _, err := c.Load("memory/team/huge"); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("the unparseable document was served anyway: %v", err)
	}
}

// TestReloadMemory_RefusesAStoreItCannotProbeCheaply: a backend with no
// fingerprint is refused at runtime, matching the config-time refusal.
func TestReloadMemory_RefusesAStoreItCannotProbeCheaply(t *testing.T) {
	c := memCollWithStore(t, "notes", nil)
	if _, err := c.ReloadMemory(context.Background()); err == nil {
		t.Fatal("a local memory store should not be reconcilable")
	}
	bare := FromPages("notes", nil)
	if _, err := bare.ReloadMemory(context.Background()); err == nil {
		t.Fatal("a collection with no memory store should refuse to reconcile")
	}
}

// --- targets ------------------------------------------------------------

func TestRefreshTargets(t *testing.T) {
	both := gcsSource("")
	both.Memory = &memory.Spec{
		Type:    memory.BackendGCS,
		Bucket:  "example-kb",
		Prefix:  "handbook/memory/",
		Refresh: &refresh.Spec{Interval: refresh.Duration(15 * time.Second)},
	}
	if err := both.Validate(); err != nil {
		t.Fatalf("the test's own source is invalid: %v", err)
	}
	pinned := gcsSource("")
	pinned.Prefix, pinned.Object, pinned.Generation, pinned.Refresh = "", "kb.tar.gz", 42, nil

	// Built directly rather than through Open, which would go on to OPEN
	// the memory store — a real ADC-backed GCS client. What is under test
	// here is which targets a configuration produces, not how a store is
	// dialled.
	cols := make([]*Collection, 0, 3)
	for _, spec := range []struct {
		name string
		src  contentsource.Source
	}{
		{"static", contentsource.Source{Type: contentsource.TypeLocal, Layout: contentsource.MergeLayout(contentsource.Layout{})}},
		{"handbook", both},
		{"archive", pinned},
	} {
		c := FromPages(spec.name, nil)
		c.Source = spec.src
		c.status.configure(spec.src)
		cols = append(cols, c)
	}
	reg, err := New(cols...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	targets := reg.RefreshTargets()
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want a content and a memory target for the one refreshable collection", len(targets))
	}
	for i, want := range []refresh.Key{
		{Ordinal: 1, Kind: refresh.KindContent, Name: "handbook"},
		{Ordinal: 1, Kind: refresh.KindMemory, Name: "handbook"},
	} {
		if got := targets[i].Key(); got != want {
			t.Errorf("targets[%d].Key() = %+v, want %+v", i, got, want)
		}
	}
	if got := targets[0].Spec().Every(); got != time.Minute {
		t.Errorf("content interval = %s, want 1m", got)
	}
	if got := targets[1].Spec().Every(); got != 15*time.Second {
		t.Errorf("memory interval = %s, want 15s", got)
	}

	// A per-request VIEW owns nothing and reconciles nothing.
	if got := reg.Restrict(only("handbook")).RefreshTargets(); got != nil {
		t.Errorf("a restricted view produced %d targets, want none", len(got))
	}
	if got := reg.ViewedBy(kb.AsOwner(aliceNS)).RefreshTargets(); got != nil {
		t.Errorf("a viewer-scoped view produced %d targets, want none", len(got))
	}

	// The status surface exists for the configured targets and nothing
	// else.
	handbook, err := reg.Get("handbook")
	if err != nil {
		t.Fatal(err)
	}
	statuses := handbook.ReloadStatuses()
	if len(statuses) != 2 {
		t.Fatalf("ReloadStatuses() = %+v, want one per configured target", statuses)
	}
	if statuses[0].Policy != refresh.PolicyServeLastGood {
		t.Errorf("policy = %q, want the serve-last-good default", statuses[0].Policy)
	}
	archive, err := reg.Get("archive")
	if err != nil {
		t.Fatal(err)
	}
	if got := archive.ReloadStatuses(); len(got) != 0 {
		t.Errorf("a pinned collection reported %+v, want no refresh status", got)
	}
}

// --- concurrency --------------------------------------------------------

// TestReload_IsRaceFreeUnderConcurrentReads is the race story this
// change owes: snapshots are swapped repeatedly while search, show, list
// and memory writes run against the same *Collection.
//
// Two failure modes it is looking for, and only one of them is a data
// race the detector would find on its own. The other is a USE-AFTER-
// CLOSE: a query holding an index that a swap closed underneath it,
// which shows up here as a search error rather than as a race report.
// Both are failures.
func TestReload_IsRaceFreeUnderConcurrentReads(t *testing.T) {
	ctx := context.Background()
	f := newFakeSource(t)
	f.publish("gen-0", map[string]string{"wiki/shared.md": wikiPage("shared", "the axolotl handbook")})
	c := refreshableCollection(t, f, "")
	bucket := newFakeMemoryBucket()
	if err := c.AttachMemory(ctx, bucket.store()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Index(); err != nil {
		t.Fatal(err)
	}
	reg, err := New(c)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// The swapper: a new content generation, over and over.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f.publish(fmt.Sprintf("gen-%d", i+1), map[string]string{
				"wiki/shared.md":                    wikiPage("shared", "the axolotl handbook"),
				fmt.Sprintf("wiki/page-%d.md", i+1): wikiPage(fmt.Sprintf("page-%d", i+1), "generated body"),
			})
			if _, err := c.ReloadContent(ctx); err != nil && !errors.Is(err, refresh.ErrBusy) {
				t.Errorf("ReloadContent: %v", err)
				return
			}
		}
	}()

	// Readers, as several distinct principals, so the visibility clause
	// is in play throughout.
	for _, owner := range []string{aliceNS, bobNS, ""} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			view := reg.ViewedBy(kb.AsOwner(owner))
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := view.Search(ctx, "", "axolotl", 5); err != nil {
					t.Errorf("search as %q during a swap: %v", owner, err)
					return
				}
				if _, err := view.Pages(""); err != nil {
					t.Errorf("list as %q during a swap: %v", owner, err)
					return
				}
				if _, err := view.Show("", "shared"); err != nil {
					t.Errorf("show as %q during a swap: %v", owner, err)
					return
				}
			}
		}(owner)
	}

	// Writers, saving personal memories while the swaps happen.
	for _, owner := range []string{aliceNS, bobNS} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for i := 0; i < 12; i++ {
				key := personalKey(owner, fmt.Sprintf("note-%d", i))
				if _, _, err := c.SaveMemory(ctx, key,
					memoryDoc(t, "Note", "the axolotl detail"), memory.CreateOnly()); err != nil {
					t.Errorf("SaveMemory(%s) during a swap: %v", key, err)
					return
				}
			}
		}(owner)
	}

	// A memory reconcile racing the content reconciles for the same
	// collection: one of the two wins the reload slot, the other gets
	// ErrBusy, and neither corrupts anything.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := c.ReloadMemory(ctx); err != nil && !errors.Is(err, refresh.ErrBusy) {
				t.Errorf("ReloadMemory during a swap: %v", err)
				return
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Every write that was reported as saved must still be findable —
	// through the overlay AND through whichever index is now installed.
	for _, owner := range []string{aliceNS, bobNS} {
		view := reg.ViewedBy(kb.AsOwner(owner))
		listed, err := view.Pages("")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 12; i++ {
			id := personalID(owner, fmt.Sprintf("note-%d", i))
			if _, err := view.Show("", id); err != nil {
				t.Errorf("a memory saved during the swaps is gone from show: %s: %v", id, err)
			}
			if !hasID(listed, id) {
				t.Errorf("a memory saved during the swaps is gone from list: %s", id)
			}
		}
		hits, err := view.Search(ctx, "", "axolotl", 100)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, h := range hits {
			if strings.HasPrefix(h.Page.ID, kb.PrivatePrefix+owner+"/") {
				found++
			}
		}
		if found != 12 {
			t.Errorf("%s finds %d of their 12 memories in the post-swap index", owner, found)
		}
		// And still sees none of the other principal's.
		for _, h := range hits {
			if strings.HasPrefix(h.Page.ID, kb.PrivatePrefix) && !strings.HasPrefix(h.Page.ID, kb.PrivatePrefix+owner+"/") {
				t.Errorf("%s can see %q after the swaps", owner, h.Page.ID)
			}
		}
	}
}

// --- helpers ------------------------------------------------------------

// memoryRefreshSource is the content-source.yaml entry a collection with
// a reconcilable memory store has.
func memoryRefreshSource() contentsource.Source {
	return contentsource.Source{
		Type:   contentsource.TypeLocal,
		Path:   ".",
		Layout: contentsource.MergeLayout(contentsource.Layout{}),
		Memory: &memory.Spec{
			Type:    memory.BackendGCS,
			Bucket:  "example-kb",
			Prefix:  "kb/memory/",
			Refresh: &refresh.Spec{Interval: refresh.Duration(15 * time.Second)},
		},
	}
}

// fakeMemoryBucket is one shared object store with GCS generation
// semantics, handing out an independent memory.Store per replica. It is
// deliberately shared state with per-replica handles: that is exactly
// the shape two hosted processes over one bucket are in.
type fakeMemoryBucket struct {
	mu      sync.Mutex
	objects map[string]fakeMemoryObject
	nextGen int64
	// loadCount counts full document re-reads, so a test can prove an
	// unchanged fingerprint did not cause one.
	loadCount int
	// err, when set, fails every operation.
	err error
	// loadGate, when non-nil, is received from inside Load.
	loadGate chan struct{}
	// loadEntered is closed the first time Load is entered while gated.
	loadEntered chan struct{}
}

type fakeMemoryObject struct {
	body []byte
	gen  int64
}

func newFakeMemoryBucket() *fakeMemoryBucket {
	return &fakeMemoryBucket{objects: map[string]fakeMemoryObject{}, nextGen: 1000}
}

func (b *fakeMemoryBucket) store() memory.Store { return &fakeMemoryStore{bucket: b} }

func (b *fakeMemoryBucket) loads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loadCount
}

func (b *fakeMemoryBucket) failWith(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *fakeMemoryBucket) gateLoad(gate chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loadGate = gate
	b.loadEntered = make(chan struct{})
}

func (b *fakeMemoryBucket) waitForLoad(t *testing.T) {
	t.Helper()
	b.mu.Lock()
	entered := b.loadEntered
	b.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reload never reached the store's Load")
	}
}

// fakeMemoryStore is one replica's handle on the shared bucket.
type fakeMemoryStore struct{ bucket *fakeMemoryBucket }

func (s *fakeMemoryStore) Describe() string         { return "gcs://example-kb/kb/memory/" }
func (s *fakeMemoryStore) Location(k string) string { return "gs://example-kb/kb/memory/" + k }

func (s *fakeMemoryStore) Load(context.Context) ([]memory.Record, error) {
	b := s.bucket
	b.mu.Lock()
	if b.err != nil {
		err := b.err
		b.mu.Unlock()
		return nil, err
	}
	b.loadCount++
	gate, entered := b.loadGate, b.loadEntered
	keys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		keys = append(keys, k)
	}
	b.mu.Unlock()

	if gate != nil {
		if entered != nil {
			select {
			case <-entered:
			default:
				close(entered)
			}
		}
		<-gate
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]memory.Record, 0, len(keys))
	for _, k := range keys {
		if memory.Staged(k) {
			continue
		}
		o, ok := b.objects[k]
		if !ok {
			continue
		}
		out = append(out, memory.Record{Key: k, Body: o.body, Version: memory.Version(fmt.Sprint(o.gen))})
	}
	return out, nil
}

func (s *fakeMemoryStore) Fingerprint(context.Context) (string, error) {
	b := s.bucket
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return "", b.err
	}
	keys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		if memory.Staged(k) {
			continue
		}
		keys = append(keys, k)
	}
	sortStrings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s\x00%d\n", k, b.objects[k].gen)
	}
	return sb.String(), nil
}

func (s *fakeMemoryStore) Stat(_ context.Context, key string) (memory.Version, bool, error) {
	b := s.bucket
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[key]
	if !ok {
		return "", false, nil
	}
	return memory.Version(fmt.Sprint(o.gen)), true, nil
}

func (s *fakeMemoryStore) Put(_ context.Context, key string, body []byte, pre memory.Precondition) (memory.Version, error) {
	b := s.bucket
	b.mu.Lock()
	defer b.mu.Unlock()
	o, exists := b.objects[key]
	switch {
	case pre.Absent && exists:
		return "", &memory.ConflictError{Key: key, Current: memory.Version(fmt.Sprint(o.gen))}
	case !pre.Absent && !exists:
		return "", &memory.ConflictError{Key: key}
	case !pre.Absent && string(pre.Version) != fmt.Sprint(o.gen):
		return "", &memory.ConflictError{Key: key, Current: memory.Version(fmt.Sprint(o.gen))}
	}
	b.nextGen++
	b.objects[key] = fakeMemoryObject{body: append([]byte(nil), body...), gen: b.nextGen}
	return memory.Version(fmt.Sprint(b.nextGen)), nil
}

func (s *fakeMemoryStore) Stage(_ context.Context, key string, body []byte) (string, error) {
	b := s.bucket
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextGen++
	b.objects[key] = fakeMemoryObject{body: append([]byte(nil), body...), gen: b.nextGen}
	return s.Location(key), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// captureCollectionStderr collects what fn writes to os.Stderr — the
// skip-a-malformed-document warning goes there directly.
func captureCollectionStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 512)
		for {
			n, rerr := r.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if rerr != nil {
				break
			}
		}
		done <- string(buf)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
