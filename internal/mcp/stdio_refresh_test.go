package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// stdio_refresh_test.go pins the stdio half of meerkat-mob#25: `mk mcp
// serve` runs a refresh controller over the scheduled targets, and none at
// all when no collection carries a refresh: block (MK-FRESH-09).

func openLocalCollections(t *testing.T, withRefresh ...bool) *collections.Registry {
	t.Helper()
	var resolved []contentsource.ResolvedCollection
	for i, refreshed := range withRefresh {
		dir := t.TempDir()
		page := filepath.Join(dir, "wiki", "p.md")
		if err := os.MkdirAll(filepath.Dir(page), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(page, []byte("---\nid: p\ntitle: P\n---\n# P\n\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		src := contentsource.Source{Type: contentsource.TypeLocal, Path: dir, Layout: contentsource.Layout{Wiki: "wiki"}}
		if refreshed {
			src.Refresh = &refresh.Spec{Interval: refresh.Duration(time.Minute)}
		}
		resolved = append(resolved, contentsource.ResolvedCollection{
			Name: string(rune('a' + i)), Dir: dir, Provenance: "disk:" + dir, Source: src,
		})
	}
	reg, err := collections.Open(context.Background(), resolved)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// No refresh: block anywhere: no controller. A nil controller is what
// guarantees no goroutine and no timer, since Start on nil returns at
// once. The local collections still have their manual-only targets, and
// those must not be enough to start one.
func TestStdioRefresh_NoBlockNoController(t *testing.T) {
	reg := openLocalCollections(t, false, false)
	if n := len(reg.RefreshTargets()); n != 2 {
		t.Fatalf("precondition: RefreshTargets = %d, want the two manual local targets", n)
	}
	if ctl := stdioRefresh(reg, nil); ctl != nil {
		t.Fatalf("stdio started a refresh controller over %d target(s) with no refresh: block configured", ctl.Targets())
	}
}

// One block: a controller over exactly that collection's target.
func TestStdioRefresh_ABlockStartsAControllerForItAlone(t *testing.T) {
	reg := openLocalCollections(t, true, false)
	ctl := stdioRefresh(reg, nil)
	if ctl == nil {
		t.Fatal("a refresh: block is configured but stdio has no controller")
	}
	if n := ctl.Targets(); n != 1 {
		t.Errorf("controller targets = %d, want only the scheduled one", n)
	}
}

// stdio schedules LOCAL targets only. An object store's or memory store's
// refresh: block has never polled under `mk mcp serve`, and starting to
// would be new credentialed bucket traffic from existing configurations.
func TestStdioRefresh_ObjectAndMemoryBlocksStayUnpolled(t *testing.T) {
	local := openLocalCollections(t, true)
	lc, err := local.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	every := &refresh.Spec{Interval: refresh.Duration(time.Minute)}
	pages := []kb.Page{testPage("p", "P", "body", "", "", "")}
	bucket := collections.FromPages("bucket", pages)
	bucket.Source = contentsource.Source{Type: contentsource.TypeGCS, Bucket: "b", Prefix: "p/", Refresh: every}
	mem := collections.FromPages("mem", pages)
	mem.Source.Memory = &memory.Spec{Type: memory.BackendGCS, Bucket: "b", Prefix: "m/", Refresh: every}
	reg, err := collections.New(lc, bucket, mem)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, tg := range reg.RefreshTargets() {
		if tg.Spec() != nil {
			kinds = append(kinds, tg.Key().Name+"/"+tg.Key().Kind)
		}
	}
	if len(kinds) != 3 {
		t.Fatalf("precondition: scheduled targets = %v, want local, object store and memory", kinds)
	}
	ctl := stdioRefresh(reg, nil)
	if ctl == nil || ctl.Targets() != 1 {
		t.Fatalf("stdio controller over %d target(s), want only the local one", ctl.Targets())
	}
}
