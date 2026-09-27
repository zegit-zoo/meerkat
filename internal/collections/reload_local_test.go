package collections

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// reload_local_test.go pins issue #105: the admin trigger (SIGHUP on
// `mk mcp serve-http`, which is refresh.Controller.ReloadNow) rebuilds a
// `type: local` collection's search index from what is on disk now,
// without a restart, and without a query ever seeing an error.

// writeLocalPage writes one wiki page into a local collection's
// directory.
func writeLocalPage(t *testing.T, dir, id, body string) {
	t.Helper()
	path := filepath.Join(dir, "wiki", filepath.FromSlash(id)+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nid: "+id+"\ntitle: "+id+"\n---\n# "+id+"\n\n"+body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// openLocalPair mounts two `type: local` collections — the shape of the
// shared mk-ai server (mk-ai plus dotfiles) — and returns the registry
// and the first collection's directory.
func openLocalPair(t *testing.T) (*Registry, string) {
	t.Helper()
	var resolved []contentsource.ResolvedCollection
	var first string
	for _, name := range []string{"notes", "refs"} {
		dir := t.TempDir()
		writeLocalPage(t, dir, name+"/stable", "The stable page about lighthouses, present from the start.")
		if first == "" {
			first = dir
		}
		resolved = append(resolved, contentsource.ResolvedCollection{
			Name:       name,
			Dir:        dir,
			Provenance: "disk:" + dir,
			Source:     contentsource.Source{Type: contentsource.TypeLocal, Path: dir, Layout: contentsource.Layout{Wiki: "wiki"}},
		})
	}
	reg, err := Open(context.Background(), resolved)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg, first
}

func registrySearchIDs(t *testing.T, reg *Registry, q string) []string {
	t.Helper()
	hits, err := reg.Search(context.Background(), "", q, 10)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Collection + ":" + h.Page.ID
	}
	return out
}

// TestReloadLocal_PicksUpChangesWithoutARestart is the #105 acceptance
// test, through the exact call SIGHUP makes: a page added on disk after
// the index was built is unfindable until ReloadNow, and found after it;
// a deleted page stops being returned.
func TestReloadLocal_PicksUpChangesWithoutARestart(t *testing.T) {
	ctx := context.Background()
	reg, dir := openLocalPair(t)
	if got := registrySearchIDs(t, reg, "lighthouses"); len(got) != 2 {
		t.Fatalf("stable pages = %v, want both collections' page", got)
	}

	targets := reg.RefreshTargets()
	if len(targets) != 2 {
		t.Fatalf("RefreshTargets = %d, want one manual local target per collection", len(targets))
	}
	for _, tg := range targets {
		if tg.Spec() != nil {
			t.Errorf("%s: a local target must be manual-only (nil Spec)", tg.Key().Name)
		}
	}
	ctl := refresh.New(refresh.Options{Targets: targets})

	writeLocalPage(t, dir, "notes/zebrafish", "The zebrafish page, written after startup.")
	if got := registrySearchIDs(t, reg, "zebrafish"); len(got) != 0 {
		t.Fatalf("precondition: the startup index already has the new page: %v", got)
	}

	if err := ctl.ReloadNow(ctx); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	if got := registrySearchIDs(t, reg, "zebrafish"); len(got) != 1 || got[0] != "notes:notes/zebrafish" {
		t.Fatalf("after reload, zebrafish = %v, want [notes:notes/zebrafish]", got)
	}

	if err := os.Remove(filepath.Join(dir, "wiki", "notes", "zebrafish.md")); err != nil {
		t.Fatal(err)
	}
	if err := ctl.ReloadNow(ctx); err != nil {
		t.Fatalf("ReloadNow after delete: %v", err)
	}
	if got := registrySearchIDs(t, reg, "zebrafish"); len(got) != 0 {
		t.Errorf("a deleted page is still returned after reload: %v", got)
	}
	if got := registrySearchIDs(t, reg, "lighthouses"); len(got) != 2 {
		t.Errorf("the untouched pages were lost by the rebuild: %v", got)
	}
}

// TestReloadLocal_QueriesDuringSwapsNeverError hammers search from
// several goroutines while the index is rebuilt and swapped repeatedly.
// Every query must succeed and answer from a whole index: the stable
// page is always there, and the page added before the rebuilds is either
// absent (old index) or present once (new index), never an error.
func TestReloadLocal_QueriesDuringSwapsNeverError(t *testing.T) {
	ctx := context.Background()
	reg, dir := openLocalPair(t)
	ctl := refresh.New(refresh.Options{Targets: reg.RefreshTargets()})
	writeLocalPage(t, dir, "notes/zebrafish", "The zebrafish page, written after startup.")

	stop := make(chan struct{})
	var queries atomic.Int64
	var wg sync.WaitGroup
	errc := make(chan error, 16)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				stable, err := reg.Search(ctx, "", "lighthouses", 10)
				if err != nil {
					errc <- err
					return
				}
				if len(stable) != 2 {
					errc <- errors.New("a query during a swap lost the stable pages")
					return
				}
				fresh, err := reg.Search(ctx, "", "zebrafish", 10)
				if err != nil {
					errc <- err
					return
				}
				if len(fresh) > 1 {
					errc <- errors.New("a query during a swap saw the new page twice")
					return
				}
				queries.Add(2)
			}
		}()
	}
	for range 25 {
		if err := ctl.ReloadNow(ctx); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("ReloadNow: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if queries.Load() == 0 {
		t.Fatal("no query ran during the rebuilds; the test proved nothing")
	}
	if got := registrySearchIDs(t, reg, "zebrafish"); len(got) != 1 {
		t.Errorf("after the rebuilds, zebrafish = %v, want one hit", got)
	}
}

// A second trigger while a rebuild holds the slot is skipped, not queued
// and not an error — the same contract as an object-store reload.
func TestReloadLocal_ConcurrentTriggerIsBusy(t *testing.T) {
	reg, _ := openLocalPair(t)
	c, err := reg.Get("notes")
	if err != nil {
		t.Fatalf("notes not mounted: %v", err)
	}
	c.reloadMu.Lock()
	_, err = c.ReloadLocal(context.Background())
	c.reloadMu.Unlock()
	if !errors.Is(err, refresh.ErrBusy) {
		t.Fatalf("ReloadLocal with the slot held = %v, want ErrBusy", err)
	}
}
