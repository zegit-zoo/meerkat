package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// freshness_test.go pins meerkat-mob#25 part C on the MCP surface: the
// freshness record in mk_list_collections, the advisory line after a
// search or show result, and the freshness gauge.

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeWikiPage(t *testing.T, dir, id, body string) {
	t.Helper()
	p := filepath.Join(dir, "wiki", id+".md")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("---\nid: "+id+"\ntitle: "+id+"\n---\n# "+id+"\n\n"+body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// staleRegistry mounts "kb", a git working tree whose remote is one
// commit ahead (behind-remote once probed), and "plain", a local
// collection without a refresh: block. onDivergence is kb's policy. It
// returns the registry, the remote tip and kb's working tree.
func staleRegistry(t *testing.T, onDivergence string) (*collections.Registry, string, string) {
	t.Helper()
	fx := newStaleFixture(t, onDivergence, "kb", "plain")
	return fx.reg, fx.tip, fx.dir
}

// staleFixture is staleRegistry with everything a disclosure test needs
// to look for: both names, every directory, the bare remote and both
// commits.
type staleFixture struct {
	reg                 *collections.Registry
	kbName, plainName   string
	dir, plainDir, bare string
	head, tip           string
}

func newStaleFixture(t *testing.T, onDivergence, kbName, plainName string) *staleFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Cleanup(collections.UseFileRemotesForTest())
	seed := t.TempDir()
	gitIn(t, seed, "init", "-q")
	writeWikiPage(t, seed, "notes/lighthouse", "About lighthouses.")
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "kb.git")
	gitIn(t, seed, "clone", "-q", "--bare", seed, bare)
	dir := filepath.Join(t.TempDir(), "kb")
	gitIn(t, t.TempDir(), "clone", "-q", bare, dir)
	head := gitIn(t, dir, "rev-parse", "HEAD")
	pusher := filepath.Join(t.TempDir(), "pusher")
	gitIn(t, t.TempDir(), "clone", "-q", bare, pusher)
	writeWikiPage(t, pusher, "notes/zebrafish", "About zebrafish.")
	gitIn(t, pusher, "add", ".")
	gitIn(t, pusher, "commit", "-q", "-m", "ahead")
	gitIn(t, pusher, "push", "-q", "origin", "main")
	tip := gitIn(t, pusher, "rev-parse", "HEAD")

	plain := t.TempDir()
	writeWikiPage(t, plain, "other/page", "An unrelated page.")
	reg, err := collections.Open(context.Background(), []contentsource.ResolvedCollection{
		{Name: kbName, Dir: dir, Provenance: "disk:" + dir, Source: contentsource.Source{
			Type: contentsource.TypeLocal, Path: dir, Layout: contentsource.Layout{Wiki: "wiki"},
			Refresh: &refresh.Spec{Interval: refresh.Duration(time.Minute), RemoteCheck: refresh.Duration(time.Minute), OnDivergence: onDivergence},
		}},
		{Name: plainName, Dir: plain, Provenance: "disk:" + plain, Source: contentsource.Source{
			Type: contentsource.TypeLocal, Path: plain, Layout: contentsource.Layout{Wiki: "wiki"},
		}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	kb, err := reg.Get(kbName)
	if err != nil {
		t.Fatal(err)
	}
	// ProbeRemote never pulls, so even a pull policy stays behind-remote.
	if _, err := kb.ProbeRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f, _ := kb.Freshness(); f.State != collections.FreshBehindRemote {
		t.Fatalf("precondition: kb freshness = %+v, want behind-remote", f)
	}
	return &staleFixture{reg: reg, kbName: kbName, plainName: plainName,
		dir: dir, plainDir: plain, bare: bare, head: head, tip: tip}
}

func TestListCollections_CarriesFreshnessOnlyWhereConfigured(t *testing.T) {
	reg, tip, _ := staleRegistry(t, "")
	body, err := listCollectionsJSON(context.Background(), reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Name      string                 `json:"name"`
		Freshness *collections.Freshness `json:"freshness"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		switch c.Name {
		case "kb":
			if c.Freshness == nil || c.Freshness.State != collections.FreshBehindRemote || c.Freshness.Remote != tip {
				t.Errorf("kb freshness = %+v, want behind-remote at %s", c.Freshness, tip)
			}
		case "plain":
			if c.Freshness != nil {
				t.Errorf("plain has no refresh: block but carries freshness %+v", c.Freshness)
			}
		}
	}
	if strings.Contains(body, `"freshness": null`) {
		t.Error("an absent record rendered as null; it must be omitted")
	}
}

func invokeTool(t *testing.T, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil || res == nil || res.IsError {
		t.Fatalf("%s: %v %+v", name, err, res)
	}
	return res
}

func texts(res *mcp.CallToolResult) []string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

var hexRun = regexp.MustCompile(`[0-9a-f]{12,}`)

// The advisory is a SECOND text item, after the result, sent once per
// (session, collection, state), bounded, and never naming a commit.
func TestAdvisory_SecondItemOncePerSessionAndState(t *testing.T) {
	reg, _, _ := staleRegistry(t, "")
	opts := transportOptions{Advice: newAdvisories()}
	search := searchHandler(reg, opts)

	first := texts(invokeTool(t, search, toolSearch, map[string]any{"query": "lighthouses"}))
	if len(first) != 2 {
		t.Fatalf("first search: %d text items, want the result then one advisory: %q", len(first), first)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(first[0]), &hits); err != nil {
		t.Fatalf("the FIRST item is no longer the result array: %v\n%s", err, first[0])
	}
	adv := first[1]
	if !strings.Contains(adv, `"kb"`) || !strings.Contains(adv, "behind-remote") || strings.Contains(adv, "\n") {
		t.Errorf("advisory = %q, want one line naming kb and behind-remote", adv)
	}
	if len(adv) > collections.MaxAdvisory || hexRun.MatchString(adv) {
		t.Errorf("advisory is over %d bytes or carries a hash: %q", collections.MaxAdvisory, adv)
	}

	if again := texts(invokeTool(t, search, toolSearch, map[string]any{"query": "lighthouses"})); len(again) != 1 {
		t.Errorf("the same session was advised twice: %q", again)
	}
	// An explicit session_id is its own session (the mk_report_outcome key).
	if other := texts(invokeTool(t, search, toolSearch, map[string]any{"query": "lighthouses", "session_id": "s2"})); len(other) != 2 {
		t.Errorf("a new session_id was not advised: %q", other)
	}
	// mk_show advises for the page's collection, once for its session.
	show := showHandler(reg, opts)
	if got := texts(invokeTool(t, show, toolShow, map[string]any{"id": "notes/lighthouse", "collection": "kb", "session_id": "s3"})); len(got) != 2 {
		t.Errorf("mk_show in a new session: %q, want the page then one advisory", got)
	}
	// A current collection is never advised.
	if got := texts(invokeTool(t, search, toolSearch, map[string]any{"query": "unrelated", "collection": "plain", "session_id": "s4"})); len(got) != 1 {
		t.Errorf("a collection without a stale record was advised: %q", got)
	}
}

// A new state is a new advisory: kb goes from behind-remote to dirty
// (a pull refused on an edited tree) within one session, and is advised
// again. And a key suppresses only for the TTL.
func TestAdvisory_NewStateAndExpiryReAdvise(t *testing.T) {
	reg, _, dir := staleRegistry(t, refresh.DivergencePull)
	kb, err := reg.Get("kb")
	if err != nil {
		t.Fatal(err)
	}
	cols := []*collections.Collection{kb}
	a := newAdvisories()
	now := time.Now()
	a.now = func() time.Time { return now }
	if got := a.take("s", cols); len(got) != 1 || !strings.Contains(got[0], "behind-remote") {
		t.Fatalf("first take = %q, want one behind-remote advisory", got)
	}
	writeWikiPage(t, dir, "notes/lighthouse", "An uncommitted edit.")
	if _, err := kb.CheckRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f, _ := kb.Freshness(); f.State != collections.FreshDirty {
		t.Fatalf("precondition: kb freshness = %+v, want dirty", f)
	}
	if got := a.take("s", cols); len(got) != 1 || !strings.Contains(got[0], "dirty") {
		t.Fatalf("the new state was not advised: %q", got)
	}
	if got := a.take("s", cols); len(got) != 0 {
		t.Fatalf("repeated within the TTL: %q", got)
	}
	now = now.Add(advisoryTTL)
	if got := a.take("s", cols); len(got) != 1 {
		t.Errorf("not re-advised once the TTL passed: %q", got)
	}
}

func staleRecords(n int) []record {
	recs := make([]record, n)
	for i := range recs {
		recs[i] = record{name: "c" + strconv.Itoa(i), f: collections.Freshness{State: collections.FreshBehindRemote}}
	}
	return recs
}

// One result carries at most maxAdvised advisories; the rest come on
// the next result (#118 review N1).
func TestAdvisory_AtMostMaxAdvisedPerResult(t *testing.T) {
	a := newAdvisories()
	recs := staleRecords(maxAdvised + 2)
	if got := a.takeRecords("s", recs); len(got) != maxAdvised {
		t.Fatalf("first result carried %d advisories, want %d", len(got), maxAdvised)
	}
	if got := a.takeRecords("s", recs); len(got) != 2 {
		t.Errorf("second result carried %d advisories, want the 2 held back", len(got))
	}
}

// At the cap the oldest key goes, not an arbitrary one; a key advised
// again moves to the back; and expired keys leave before a live one
// is evicted (#118 review N2, S1).
func TestAdvisory_EvictionIsOldestFirstAndExpiredFirst(t *testing.T) {
	a := newAdvisories()
	t0 := time.Now()
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Millisecond) }
	for i := range advisoryCap {
		a.remember("k"+strconv.Itoa(i), at(i))
	}
	a.remember("k0", at(advisoryCap)) // advised again: now the newest
	a.remember("new", at(advisoryCap+1))
	if len(a.seen) != advisoryCap {
		t.Fatalf("set holds %d keys, want the cap %d", len(a.seen), advisoryCap)
	}
	if _, ok := a.seen["k1"]; ok {
		t.Error("the oldest key (k1) survived an eviction at the cap")
	}
	for _, k := range []string{"k0", "k2", "new"} {
		if _, ok := a.seen[k]; !ok {
			t.Errorf("%s was evicted; only the oldest may go", k)
		}
	}
	// Everything but k0 and "new" has expired by now: one more key
	// clears them, rather than evicting a live key.
	a.remember("later", at(advisoryCap).Add(advisoryTTL-time.Millisecond))
	if len(a.seen) != 3 {
		t.Errorf("after expiry the set holds %d keys, want 3 (k0, new, later)", len(a.seen))
	}
}

func TestAdvisory_TrackerIsBounded(t *testing.T) {
	a := newAdvisories()
	now := time.Now()
	a.now = func() time.Time { return now }
	for i := range advisoryCap + 50 {
		a.remember(string(rune(i))+"k", now.Add(time.Duration(i)))
	}
	if len(a.seen) > advisoryCap {
		t.Errorf("the seen-set grew to %d, over %d", len(a.seen), advisoryCap)
	}
}

func gather(t *testing.T, reg *collections.Registry) map[string]float64 {
	t.Helper()
	pr := prometheus.NewRegistry()
	pr.MustRegister(newFreshnessCollector(reg))
	fams, err := pr.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			key := f.GetName()
			for _, l := range m.GetLabel() {
				key += "|" + l.GetName() + "=" + l.GetValue()
			}
			out[key] = m.GetGauge().GetValue()
		}
	}
	return out
}

// No record anywhere: the gauge emits nothing, so /metrics is unchanged.
func TestFreshnessMetric_AbsentWithoutRecords(t *testing.T) {
	reg, err := collections.New(collections.FromPages("plain", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := gather(t, reg); len(got) != 0 {
		t.Errorf("an unconfigured registry published freshness series: %v", got)
	}
}

// With a record: six series, labelled by state only, counting only the
// configured collections.
func TestFreshnessMetric_CountsByStateOnly(t *testing.T) {
	reg, tip, _ := staleRegistry(t, "")
	got := gather(t, reg)
	if len(got) != len(collections.FreshnessStates()) {
		t.Fatalf("series = %v, want one per state", got)
	}
	if got["meerkat_collection_freshness|state=behind-remote"] != 1 {
		t.Errorf("behind-remote count = %v, want 1 (plain is not counted)", got["meerkat_collection_freshness|state=behind-remote"])
	}
	states := strings.Join(collections.FreshnessStates(), " ")
	for key := range got {
		state, ok := strings.CutPrefix(key, "meerkat_collection_freshness|state=")
		if !ok || strings.Contains(state, "|") || !strings.Contains(" "+states+" ", " "+state+" ") {
			t.Errorf("series %q: the only label must be a known state (no name, path or commit)", key)
		}
		if strings.Contains(key, "kb") || strings.Contains(key, tip[:12]) {
			t.Errorf("series %q names the collection or its commit", key)
		}
	}
}

// End to end: the hosted server's own /metrics carries the gauge.
func TestHosted_MetricsCarryFreshness(t *testing.T) {
	reg, _, _ := staleRegistry(t, "")
	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: reg,
		Version:     "test",
		Logger:      slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	code, body := get(t, ts, MetricsPath)
	if code != http.StatusOK || !strings.Contains(body, `meerkat_collection_freshness{state="behind-remote"} 1`) {
		t.Errorf("metrics = %d, want the behind-remote count:\n%s", code, body)
	}
}

// BenchmarkAdvisory_TakeAtCap is the flood from #118 review S1: every
// call a new session_id, the set full, five stale collections per
// result. Each op must stay constant, not a scan of the set.
func BenchmarkAdvisory_TakeAtCap(b *testing.B) {
	a := newAdvisories()
	recs := staleRecords(maxAdvised)
	for i := range advisoryCap {
		a.takeRecords("warm"+strconv.Itoa(i), recs[:1])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		a.takeRecords("s"+strconv.Itoa(i), recs)
	}
}
