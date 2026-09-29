package collections

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/refresh"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// ageTree backdates every file under dir by an hour, so a token taken
// over it is settled (settleWindow) and an unchanged tree really is
// unchanged to the probe.
func ageTree(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(p, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// reload_local_refresh_test.go pins meerkat-mob#25 part A: a `type:
// local` collection with a `refresh:` block is a SCHEDULED target whose
// cycle probes a (path, size, mtime) fingerprint and rebuilds only when
// it moved, and it keeps a freshness record. A collection without the
// block keeps #105's behaviour exactly: manual-only, and no record.

// openLocalRefreshPair mounts "notes" WITH a refresh: block and "refs"
// without one. The Spec is built by hand, below the config's 5s floor, so
// a scheduled test does not have to wait five seconds; Open does not
// re-validate it.
func openLocalRefreshPair(t *testing.T, every time.Duration) (*Registry, string) {
	t.Helper()
	var resolved []contentsource.ResolvedCollection
	var notes string
	for _, name := range []string{"notes", "refs"} {
		dir := t.TempDir()
		writeLocalPage(t, dir, name+"/stable", "The stable page about lighthouses, present from the start.")
		ageTree(t, dir)
		src := contentsource.Source{Type: contentsource.TypeLocal, Path: dir, Layout: contentsource.Layout{Wiki: "wiki"}}
		if name == "notes" {
			notes = dir
			src.Refresh = &refresh.Spec{Interval: refresh.Duration(every)}
		}
		resolved = append(resolved, contentsource.ResolvedCollection{
			Name: name, Dir: dir, Provenance: "disk:" + dir, Source: src,
		})
	}
	reg, err := Open(context.Background(), resolved)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg, notes
}

func targetFor(t *testing.T, reg *Registry, name string) refresh.Target {
	t.Helper()
	for _, tg := range reg.RefreshTargets() {
		if tg.Key().Name == name {
			return tg
		}
	}
	t.Fatalf("no refresh target for %q", name)
	return nil
}

func mustGet(t *testing.T, reg *Registry, name string) *Collection {
	t.Helper()
	c, err := reg.Get(name)
	if err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	return c
}

func TestLocalRefresh_ScheduledOnlyWithABlock(t *testing.T) {
	reg, _ := openLocalRefreshPair(t, time.Minute)
	if spec := targetFor(t, reg, "notes").Spec(); spec == nil || spec.Every() != time.Minute {
		t.Errorf("notes has a refresh: block but its target's Spec = %+v, want the block", spec)
	}
	if spec := targetFor(t, reg, "refs").Spec(); spec != nil {
		t.Errorf("refs has no refresh: block but its target is scheduled: %+v", spec)
	}
	if n := len(reg.RefreshTargets()); n != 2 {
		t.Errorf("RefreshTargets = %d, want one local target per collection and nothing else", n)
	}
}

// MK-FRESH-09: no block, no record, no status. MK-FRESH-01: with one, the
// mount stamps the token the first index is built from.
func TestLocalRefresh_FreshnessRecordOnlyWithABlock(t *testing.T) {
	reg, _ := openLocalRefreshPair(t, time.Minute)
	if f, ok := mustGet(t, reg, "refs").Freshness(); ok {
		t.Errorf("refs has no refresh: block but reports freshness %+v", f)
	}
	if st := mustGet(t, reg, "refs").ReloadStatuses(); len(st) != 0 {
		t.Errorf("refs has no refresh: block but reports reload status %+v", st)
	}
	notes := mustGet(t, reg, "notes")
	f, ok := notes.Freshness()
	if !ok {
		t.Fatal("notes has a refresh: block but no freshness record")
	}
	if f.State != FreshCurrent || f.Loaded == "" || f.Loaded != f.OnDisk || f.Behind || f.CheckedAt.IsZero() {
		t.Errorf("freshness after mount = %+v, want current with loaded == on_disk", f)
	}
	if f.Remote != RemoteUnknown {
		t.Errorf("remote = %q, want %q until a remote check exists", f.Remote, RemoteUnknown)
	}
	if notes.currentVersion() != f.Loaded {
		t.Errorf("the snapshot carries %q, the record says %q", notes.currentVersion(), f.Loaded)
	}
}

// The common case costs a stat walk and nothing else: no parse, no
// build, no swap.
func TestLocalRefresh_UnchangedTreeRebuildsNothing(t *testing.T) {
	ctx := context.Background()
	reg, _ := openLocalRefreshPair(t, time.Minute)
	notes := mustGet(t, reg, "notes")
	if got := registrySearchIDs(t, reg, "lighthouses"); len(got) != 2 {
		t.Fatalf("stable pages = %v", got)
	}
	before := notes.builtIndex()

	out, err := targetFor(t, reg, "notes").Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Changed {
		t.Error("an unchanged tree was rebuilt")
	}
	if notes.builtIndex() != before {
		t.Error("an unchanged tree swapped in a new index")
	}
	if f, _ := notes.Freshness(); f.State != FreshCurrent {
		t.Errorf("state = %s, want current", f.State)
	}
}

// Without a block, #105's contract holds: the signal is the change
// detection, so the manual target always rebuilds.
func TestLocalRefresh_ManualTargetStillAlwaysRebuilds(t *testing.T) {
	reg, _ := openLocalRefreshPair(t, time.Minute)
	out, err := targetFor(t, reg, "refs").Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Changed {
		t.Error("the manual-only target skipped a rebuild; SIGHUP must always rebuild")
	}
}

// The acceptance rows of meerkat-mob#25 (MK-FRESH-07): after one cycle,
// search, show and the page list agree for an add, an edit and a delete.
// None of the three disagreements in the issue's table survives it.
func TestLocalRefresh_AddEditDeleteAgreeAfterOneCycle(t *testing.T) {
	ctx := context.Background()
	reg, dir := openLocalRefreshPair(t, time.Minute)
	notes := mustGet(t, reg, "notes")
	target := targetFor(t, reg, "notes")
	cycle := func(wantChanged bool) {
		t.Helper()
		out, err := target.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if out.Changed != wantChanged {
			t.Fatalf("Changed = %v, want %v", out.Changed, wantChanged)
		}
		if out.Version != "" {
			t.Fatalf("Outcome.Version = %q: a local token must not reach the cycle span (MK-FRESH-10)", out.Version)
		}
		if f, _ := notes.Freshness(); f.State != FreshCurrent || f.Loaded != out.LogVersion {
			t.Fatalf("freshness after the cycle = %+v, want current at %s", f, out.LogVersion)
		}
	}
	pageCount := func() int {
		t.Helper()
		refs, err := reg.Pages("notes")
		if err != nil {
			t.Fatalf("Pages: %v", err)
		}
		return len(refs)
	}

	// Added: findable, showable, counted. The terms searched for below
	// are in the body only, never in the page's ID or title.
	writeLocalPage(t, dir, "notes/fauna", "The zebrafish page, written after startup.")
	cycle(true)
	if got := registrySearchIDs(t, reg, "zebrafish"); !slices.Equal(got, []string{"notes:notes/fauna"}) {
		t.Errorf("added page: search = %v", got)
	}
	if _, err := reg.Show("notes", "notes/fauna"); err != nil {
		t.Errorf("added page: show = %v", err)
	}
	if n := pageCount(); n != 2 {
		t.Errorf("added page: count = %d, want 2", n)
	}

	// Edited: the old term is gone from search, the new one hits.
	writeLocalPage(t, dir, "notes/fauna", "Rewritten: now it is about quokkas instead.")
	cycle(true)
	if got := registrySearchIDs(t, reg, "zebrafish"); len(got) != 0 {
		t.Errorf("edited page: the pre-edit term still hits: %v", got)
	}
	if got := registrySearchIDs(t, reg, "quokkas"); !slices.Equal(got, []string{"notes:notes/fauna"}) {
		t.Errorf("edited page: search for the new term = %v", got)
	}

	// Deleted: not found by search, not shown, not counted.
	if err := os.Remove(filepath.Join(dir, "wiki", "notes", "fauna.md")); err != nil {
		t.Fatal(err)
	}
	ageTree(t, dir)
	cycle(true)
	if got := registrySearchIDs(t, reg, "quokkas"); len(got) != 0 {
		t.Errorf("deleted page: search still returns it: %v", got)
	}
	if _, err := reg.Show("notes", "notes/fauna"); err == nil {
		t.Error("deleted page: still shown")
	}
	if n := pageCount(); n != 1 {
		t.Errorf("deleted page: count = %d, want 1", n)
	}

	// And a quiet cycle after all that is quiet.
	cycle(false)
}

// A probe that cannot read the tree leaves the last index serving, marks
// the collection degraded and the record unknown, and the next good
// probe clears both.
func TestLocalRefresh_FailedProbeServesLastGood(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory; the probe cannot be made to fail")
	}
	ctx := context.Background()
	reg, dir := openLocalRefreshPair(t, time.Minute)
	notes := mustGet(t, reg, "notes")
	// Build the index first, as a server's startup does: it is the
	// last-good snapshot the failure must leave serving.
	if got := registrySearchIDs(t, reg, "lighthouses"); len(got) != 2 {
		t.Fatalf("stable pages = %v", got)
	}
	sub := filepath.Join(dir, "wiki", "notes")
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o750) })

	if _, err := targetFor(t, reg, "notes").Reconcile(ctx); err == nil {
		t.Fatal("a probe of an unreadable tree succeeded")
	}
	if got := registrySearchIDs(t, reg, "lighthouses"); len(got) != 2 {
		t.Errorf("the last known-good index stopped serving: %v", got)
	}
	if f, _ := notes.Freshness(); f.State != FreshUnknown || f.OnDisk != "" || f.Loaded == "" {
		t.Errorf("freshness after a failed probe = %+v, want unknown, keeping loaded", f)
	}
	if reason, _ := notes.status.degraded(); reason == "" {
		t.Error("a failed probe did not mark the collection degraded")
	}

	if err := os.Chmod(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	out, err := targetFor(t, reg, "notes").Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after recovery: %v", err)
	}
	if out.Changed {
		t.Error("recovery rebuilt an unchanged tree")
	}
	if f, _ := notes.Freshness(); f.State != FreshCurrent {
		t.Errorf("freshness after recovery = %+v, want current", f)
	}
	if reason, _ := notes.status.degraded(); reason != "" {
		t.Errorf("still degraded after a good probe: %s", reason)
	}
}

// The scheduled path end to end: a controller started over the targets
// picks a new page up within an interval, with nobody calling anything.
func TestLocalRefresh_TheScheduleFindsANewPage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg, dir := openLocalRefreshPair(t, 20*time.Millisecond)
	ctl := refresh.New(refresh.Options{Targets: reg.RefreshTargets()})
	ctl.Start(ctx)
	defer func() { _ = ctl.Close() }()

	writeLocalPage(t, dir, "notes/zebrafish", "The zebrafish page, written after startup.")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := registrySearchIDs(t, reg, "zebrafish"); len(got) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the scheduled refresh never made the new page findable")
}

// A rebuild moves the snapshot's token, and with it the link graph's
// key, so backlinks follow the rebuild. Before, a local rebuild kept the
// old (empty) version and the graph served its cached backlinks forever.
func TestReloadLocal_BacklinksFollowTheRebuild(t *testing.T) {
	ctx := context.Background()
	reg, _ := openLocalRefreshPair(t, time.Minute)
	refsDir := mustGet(t, reg, "refs").Source.Path
	stable, err := reg.Show("refs", "refs/stable")
	if err != nil {
		t.Fatal(err)
	}
	if pl := reg.LinksOf(stable); len(pl.LinkedFrom) != 0 {
		t.Fatalf("precondition: backlinks = %v", pl.LinkedFrom)
	}

	path := filepath.Join(refsDir, "wiki", "refs", "linker.md")
	body := "---\nid: refs/linker\ntitle: Linker\nrelated:\n  - refs/stable\n---\n# Linker\n\nPoints at the stable page.\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// refs has no refresh: block; this is the SIGHUP path.
	if _, err := targetFor(t, reg, "refs").Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if pl := reg.LinksOf(stable); !slices.Contains(pl.LinkedFrom, "refs:refs/linker") {
		t.Errorf("backlinks after the rebuild = %v, want refs:refs/linker among them", pl.LinkedFrom)
	}
}

// The state set is closed and ordered; this is the list a client may
// rely on (meerkat-mob#25). Adding a value is a deliberate edit here.
func TestFreshnessStates_TheClosedSet(t *testing.T) {
	want := []string{"unknown", "behind-disk", "diverged", "dirty", "behind-remote", "current"}
	if got := FreshnessStates(); !slices.Equal(got, want) {
		t.Errorf("FreshnessStates = %v, want %v", got, want)
	}
}

func TestFreshness_Settle(t *testing.T) {
	cases := []struct {
		loaded, onDisk, want string
		behind               bool
	}{
		{"a", "a", FreshCurrent, false},
		{"a", "b", FreshBehindDisk, true},
		{"a", "", FreshUnknown, false},
		{"", "b", FreshUnknown, false},
	}
	for _, tc := range cases {
		f := Freshness{Loaded: tc.loaded, OnDisk: tc.onDisk}
		f.settle()
		if f.State != tc.want || f.Behind != tc.behind {
			t.Errorf("settle(loaded=%q, on_disk=%q) = %s/behind=%v, want %s/behind=%v",
				tc.loaded, tc.onDisk, f.State, f.Behind, tc.want, tc.behind)
		}
		if !slices.Contains(FreshnessStates(), f.State) {
			t.Errorf("settle produced %q, outside the closed set", f.State)
		}
	}
}

// The racy case from review: a same-size rewrite inside one kernel
// timestamp tick leaves the token unchanged. A probe that ran between the
// two writes took an UNSETTLED token, so the next probe rebuilds anyway
// and the second write is not lost.
func TestLocalRefresh_AnUnsettledTokenRebuildsOnce(t *testing.T) {
	ctx := context.Background()
	reg, dir := openLocalRefreshPair(t, time.Minute)
	target := targetFor(t, reg, "notes")
	path := filepath.Join(dir, "wiki", "notes", "fauna.md")
	tick := time.Now().Truncate(time.Second)

	writeLocalPage(t, dir, "notes/fauna", "About zebrafish.")
	if err := os.Chtimes(path, tick, tick); err != nil {
		t.Fatal(err)
	}
	if out, err := target.Reconcile(ctx); err != nil || !out.Changed {
		t.Fatalf("first write: Reconcile = %+v, %v", out, err)
	}

	// Same length, same mtime: invisible to the token.
	writeLocalPage(t, dir, "notes/fauna", "About ocelotfox.")
	if err := os.Chtimes(path, tick, tick); err != nil {
		t.Fatal(err)
	}
	out, err := target.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Changed {
		t.Fatal("an unsettled token was trusted: the same-tick rewrite was never indexed")
	}
	if got := registrySearchIDs(t, reg, "ocelotfox"); len(got) != 1 {
		t.Fatalf("after the rebuild, the rewrite is not searchable: %v", got)
	}

	// Once the pages are old, the token is trusted again.
	ageTree(t, dir)
	if out, err := target.Reconcile(ctx); err != nil || !out.Changed {
		t.Fatalf("ageing moved the token; Reconcile = %+v, %v", out, err)
	}
	if out, err := target.Reconcile(ctx); err != nil || out.Changed {
		t.Fatalf("a settled, unchanged tree was rebuilt: %+v, %v", out, err)
	}
}

func TestUnsettled_Window(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		newest time.Time
		want   bool
	}{
		{"no pages", time.Time{}, false},
		{"just written", now.Add(-10 * time.Millisecond), true},
		{"inside the window", now.Add(-1900 * time.Millisecond), true},
		{"outside the window", now.Add(-3 * time.Second), false},
		{"slightly in the future", now.Add(time.Second), true},
		{"far in the future", now.Add(time.Hour), false},
	}
	for _, tc := range cases {
		if got := unsettled(kb.Fingerprint{Token: "x", Newest: tc.newest}, now); got != tc.want {
			t.Errorf("%s: unsettled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A mount: lazy local child with a refresh: block is scheduled from the
// start: a no-op while cold, stamped when the cache mounts it, and probed
// after that. Its record reads unknown while nothing is loaded.
func TestLocalRefresh_ALazyChildIsScheduledAndStampedOnMount(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	child := treeKB(t, base, "a", "kind: KnowledgeBase\nname: a\n")
	ageTree(t, child)
	root := treeKB(t, base, "root", "kind: KnowledgeBase\nname: root\nchildren:\n"+
		"  - name: a\n    source: {type: local, path: "+child+", refresh: {interval: 1m}}\n    mount: lazy\n"+
		"  - name: b\n    source: {type: local, path: "+child+"}\n    mount: lazy\n")
	resolved, _, err := contentsource.ResolveTree(ctx, contentsource.Source{Type: contentsource.TypeLocal, Path: root, Layout: contentsource.MergeLayout(contentsource.Layout{})}, filepath.Join(base, "content-source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(ctx, resolved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	a := mustGet(t, reg, "a")
	target := targetFor(t, reg, "a")
	if target.Spec() == nil {
		t.Fatal("the lazy child's refresh: block is not scheduled")
	}
	for _, tg := range reg.RefreshTargets() {
		if tg.Key().Name == "b" {
			t.Error("a lazy child without a refresh: block got a target")
		}
	}
	if st := mustGet(t, reg, "b").ReloadStatuses(); len(st) != 0 {
		t.Errorf("a lazy child without a refresh: block advertises a schedule: %+v", st)
	}
	if f, ok := a.Freshness(); !ok || f.State != FreshUnknown {
		t.Errorf("cold child's freshness = %+v, %v; want unknown", f, ok)
	}
	if out, err := target.Reconcile(ctx); err != nil || out.Changed {
		t.Fatalf("a cold child's cycle = %+v, %v; want a quiet no-op", out, err)
	}
	if !a.IsCold() {
		t.Fatal("the cycle mounted a cold child")
	}

	if _, err := reg.Search(ctx, "a", "page", 5); err != nil {
		t.Fatalf("Search mounting a: %v", err)
	}
	if a.IsCold() {
		t.Fatal("precondition: a is still cold after a search")
	}
	if f, _ := a.Freshness(); f.State != FreshCurrent || f.Loaded == "" {
		t.Errorf("freshness after the mount = %+v, want current and stamped", f)
	}
	if out, err := target.Reconcile(ctx); err != nil || out.Changed {
		t.Errorf("an unchanged mounted child was rebuilt: %+v, %v", out, err)
	}
	writeLocalPage(t, child, "a/fresh", "A page about narwhals, added after the mount.")
	if out, err := target.Reconcile(ctx); err != nil || !out.Changed {
		t.Fatalf("a changed mounted child was not rebuilt: %+v, %v", out, err)
	}
	if hits, err := reg.Search(ctx, "a", "narwhals", 5); err != nil || len(hits) != 1 {
		t.Errorf("the new page is not searchable in the lazy child: %v, %v", hits, err)
	}
}

// Warm start mounts lazy children BEFORE the refresh targets are
// enumerated (startCache runs first on both transports). The mount's
// stamp must still reach the freshness record, so an unchanged cycle
// afterwards reads current rather than unknown (review S2a).
func TestLocalRefresh_AWarmStartedLazyChildReadsCurrent(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	child := treeKB(t, base, "a", "kind: KnowledgeBase\nname: a\n")
	ageTree(t, child)
	root := treeKB(t, base, "root", "kind: KnowledgeBase\nname: root\nchildren:\n"+
		"  - name: a\n    source: {type: local, path: "+child+", refresh: {interval: 1m}}\n    mount: lazy\n")
	resolved, _, err := contentsource.ResolveTree(ctx, contentsource.Source{Type: contentsource.TypeLocal, Path: root, Layout: contentsource.MergeLayout(contentsource.Layout{})}, filepath.Join(base, "content-source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(ctx, resolved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	a := mustGet(t, reg, "a")

	if err := reg.mount(ctx, a, telemetry.MountWarmStart, false); err != nil {
		t.Fatalf("warm-start mount: %v", err)
	}
	if f, ok := a.Freshness(); !ok || f.State != FreshCurrent || f.Loaded == "" {
		t.Errorf("after the warm-start mount, freshness = %+v, %v; want current and stamped", f, ok)
	}
	target := targetFor(t, reg, "a")
	for i := range 2 {
		out, err := target.Reconcile(ctx)
		if err != nil || out.Changed {
			t.Fatalf("unchanged cycle %d = %+v, %v", i+1, out, err)
		}
		if f, _ := a.Freshness(); f.State != FreshCurrent || f.Loaded != f.OnDisk {
			t.Errorf("unchanged cycle %d: freshness = %+v, want current", i+1, f)
		}
	}
}
