package collections

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The cache lease on a generation follows the snapshot that serves it
// (meerkat#240): a reload does not let go of the old generation while a
// reader still drains it, and lets go once the last one is done, so a
// later prune can remove it. The new generation is leased from the
// moment it is resolved.
func TestReloadContent_OldGenerationStaysLeasedWhileARequestDrainsIt(t *testing.T) {
	f := newFakeSource(t)
	f.leased = true
	f.publish("gen-1", map[string]string{"wiki/a.md": wikiPage("a", "the axolotl handbook")})
	c := refreshableCollection(t, f, "")
	f.mu.Lock()
	dir1 := f.dirs["gen-1"]
	f.mu.Unlock()
	old := f.issued[dir1]
	if !old.Active() {
		t.Fatal("the generation the collection started on is not leased")
	}

	reader := c.acquire() // an in-flight request on gen-1
	f.publish("gen-2", map[string]string{"wiki/a.md": wikiPage("a", "the pangolin handbook")})
	if _, err := c.ReloadContent(context.Background()); err != nil {
		t.Fatalf("ReloadContent: %v", err)
	}
	f.mu.Lock()
	dir2 := f.dirs["gen-2"]
	f.mu.Unlock()
	if !f.issued[dir2].Active() {
		t.Error("the generation now being served is not leased")
	}
	if !old.Active() {
		t.Fatal("gen-1's lease was dropped while a request still reads from it")
	}
	reader.release()
	if old.Active() {
		t.Error("gen-1 is still leased after its last reader finished")
	}
}

// A reload that fails after the resolve (here: the new generation's
// directory is gone before it can be mounted) releases the lease it took
// on that generation, and keeps the one on the generation still served.
func TestReloadContent_FailedMountReleasesTheNewLease(t *testing.T) {
	f := newFakeSource(t)
	f.leased = true
	f.publish("gen-1", map[string]string{"wiki/a.md": wikiPage("a", "the axolotl handbook")})
	c := refreshableCollection(t, f, "")
	f.publish("gen-2", map[string]string{"wiki/a.md": wikiPage("a", "the pangolin handbook")})
	f.afterLease = func(dir string) {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	}
	if _, err := c.ReloadContent(context.Background()); err == nil || !strings.Contains(err.Error(), "mount") {
		t.Fatalf("ReloadContent = %v, want a mount failure", err)
	}
	f.mu.Lock()
	dir1, dir2 := f.dirs["gen-1"], f.dirs["gen-2"]
	f.mu.Unlock()
	if f.issued[dir2].Active() {
		t.Error("a failed reload kept its lease on the generation it did not install")
	}
	if !f.issued[dir1].Active() {
		t.Error("a failed reload dropped the lease on the generation still being served")
	}
}
