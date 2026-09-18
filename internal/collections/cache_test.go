package collections

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// openLazyTree builds root -> {a, b, c} where a, b and c are lazy
// children of similar size, so a byte budget can be sized to hold
// exactly two of them.
func openLazyTree(t *testing.T, spec *contentsource.CacheSpec, log *traversal.Log) *Registry {
	t.Helper()
	base := t.TempDir()
	mk := func(name string) string {
		return treeKB(t, base, name, "kind: KnowledgeBase\nname: "+name+"\n")
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	root := treeKB(t, base, "root", "kind: KnowledgeBase\nname: root\nchildren:\n"+
		"  - name: a\n    source: {type: local, path: "+a+"}\n    mount: lazy\n"+
		"  - name: b\n    source: {type: local, path: "+b+"}\n    mount: lazy\n"+
		"  - name: c\n    source: {type: local, path: "+c+"}\n    mount: lazy\n")
	resolved, _, err := contentsource.ResolveTree(context.Background(), contentsource.Source{Type: contentsource.TypeLocal, Path: root, Layout: contentsource.MergeLayout(contentsource.Layout{})}, filepath.Join(base, "content-source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetCache(spec, log)
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func mountedNames(reg *Registry) string {
	var out []string
	for _, c := range reg.All() {
		if c.Lazy() && !c.IsCold() {
			out = append(out, c.Name)
		}
	}
	return strings.Join(out, ",")
}

func TestCache_BudgetCullsColdestThenOldest(t *testing.T) {
	ctx := context.Background()
	// Size one child, then budget for two.
	probe := openLazyTree(t, nil, nil)
	if _, err := probe.Search(ctx, "a", "a", 5); err != nil {
		t.Fatal(err)
	}
	one, _ := probe.Resident()
	if one <= 0 {
		t.Fatalf("resident bytes after one mount = %d", one)
	}
	// Three children at ~one each: with max 3×one and watermark 0.8 the
	// limit is 2.4×one — two fit, the third pushes it over.
	spec := &contentsource.CacheSpec{MaxBytes: contentsource.ByteSize(one * 3), HighWatermark: 0.8}
	reg := openLazyTree(t, spec, nil)

	for _, name := range []string{"a", "b"} {
		if _, err := reg.Search(ctx, name, name, 5); err != nil {
			t.Fatal(err)
		}
	}
	// Warm a again so b is the coldest.
	if _, err := reg.Search(ctx, "a", "a", 5); err != nil {
		t.Fatal(err)
	}
	if got := mountedNames(reg); got != "a,b" {
		t.Fatalf("resident = %q, want a,b", got)
	}
	// Mounting c pushes the cache over the watermark: b (coldest) goes.
	if _, err := reg.Search(ctx, "c", "c", 5); err != nil {
		t.Fatal(err)
	}
	if got := mountedNames(reg); got != "a,c" {
		t.Errorf("resident after culling = %q, want a,c (b was coldest)", got)
	}
	bytes, n := reg.Resident()
	if n != 2 || bytes > int64(spec.MaxBytes) {
		t.Errorf("resident = %d bytes, %d collections; budget %d", bytes, n, spec.MaxBytes)
	}
	// b is cold again, still declared, and mounts on the next request.
	b, _ := reg.Get("b")
	if !b.IsCold() || b.Tree.Mounted {
		t.Error("culled b must be cold and marked unmounted")
	}
	if pages, _ := b.Pages(); len(pages) != 0 {
		t.Error("a cold collection has no pages")
	}
	if _, err := reg.Search(ctx, "b", "b", 5); err != nil {
		t.Fatalf("remount b: %v", err)
	}
	// The root is never in the cache and never culled.
	root, _ := reg.Get("root")
	if root.Lazy() || root.IsCold() {
		t.Error("the root must never be lazy or cold")
	}
	if err := reg.Evict(ctx, "root"); err == nil {
		t.Error("evicting the root must be refused")
	}
	if err := reg.Evict(ctx, "b"); err != nil {
		t.Errorf("Evict(b): %v", err)
	}
	if b.IsCold() != true {
		t.Error("Evict must return b to cold")
	}
	if err := reg.Evict(ctx, "nope"); err == nil {
		t.Error("Evict(unknown) must fail")
	}
}

func TestCache_TemperatureCountsTraversals(t *testing.T) {
	ctx := context.Background()
	reg := openLazyTree(t, nil, nil)
	for i := 0; i < 3; i++ {
		if _, err := reg.Search(ctx, "a", "a", 5); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.Show("a", "a"); err != nil {
		t.Fatal(err)
	}
	a, _ := reg.Get("a")
	temp, first := a.Temperature()
	if temp != 4 || first == 0 {
		t.Errorf("a: temperature %d first %d, want 4 and a sequence", temp, first)
	}
	b, _ := reg.Get("b")
	if temp, first := b.Temperature(); temp != 0 || first != 0 {
		t.Errorf("b untouched: %d %d", temp, first)
	}
	root, _ := reg.Get("root")
	if temp, _ := root.Temperature(); temp != 0 {
		t.Errorf("root was not searched (named searches do not touch it): %d", temp)
	}
}

func TestCache_FlushAndWarmStart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	log, err := traversal.Open(ctx, &traversal.Config{Backend: "local", Path: dir, HMACKeyEnv: "MEERKAT_TEST_PATH_KEY"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := &contentsource.CacheSpec{WarmStartDays: 3}
	first := openLazyTree(t, spec, log)
	for i := 0; i < 5; i++ {
		if _, err := first.Search(ctx, "c", "c", 5); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.Search(ctx, "a", "a", 5); err != nil {
		t.Fatal(err)
	}
	if err := first.FlushTemperatures(ctx); err != nil {
		t.Fatal(err)
	}
	temps, err := log.ReadTemperatures(ctx, 3)
	if err != nil || len(temps) != 2 {
		t.Fatalf("temperatures = %v %v, want a and c", temps, err)
	}
	for h := range temps {
		if h == "a" || h == "c" {
			t.Error("temperature records must key by hashed name")
		}
	}
	if temps[log.Hash("c")].Temperature != 5 || temps[log.Hash("a")].Temperature != 1 {
		t.Errorf("temperatures = %v", temps)
	}

	// A restart: everything cold, then warm start mounts the hot paths.
	second := openLazyTree(t, spec, log)
	if got := mountedNames(second); got != "" {
		t.Fatalf("fresh registry has resident lazy collections: %q", got)
	}
	n, err := second.WarmStart(ctx, 3)
	if err != nil || n != 2 {
		t.Fatalf("WarmStart = %d %v, want 2 (a and c)", n, err)
	}
	if got := mountedNames(second); got != "a,c" {
		t.Errorf("warm-started = %q, want a,c", got)
	}
	c, _ := second.Get("c")
	if temp, _ := c.Temperature(); temp != 5 {
		t.Errorf("warm start must restore temperature: %d", temp)
	}
	// With a budget for one, only the hottest is pre-mounted.
	single := openLazyTree(t, nil, nil)
	if _, err := single.Search(ctx, "a", "a", 5); err != nil {
		t.Fatal(err)
	}
	one, _ := single.Resident()
	tight := openLazyTree(t, &contentsource.CacheSpec{WarmStartDays: 3, MaxBytes: contentsource.ByteSize(one + one/4), HighWatermark: 1}, log)
	n, _ = tight.WarmStart(ctx, 3)
	if n != 1 || mountedNames(tight) != "c" {
		t.Errorf("tight warm start = %d %q, want 1 and c", n, mountedNames(tight))
	}
	// No log: warm start is a no-op.
	if n, err := openLazyTree(t, spec, nil).WarmStart(ctx, 3); n != 0 || err != nil {
		t.Errorf("no log: %d %v", n, err)
	}
}
