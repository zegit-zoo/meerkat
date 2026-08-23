package collections

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// memory_test.go covers the overlay: a memory written through a
// collection must be listable, showable and SEARCHABLE immediately,
// with no rebuild and no restart.

// memColl returns a collection with a fresh local memory store
// attached, over one pre-existing content page.
func memColl(t *testing.T) (*Collection, memory.Store) {
	t.Helper()
	c := FromPages("notes", []kb.Page{page("handbook/onboarding", "Onboarding", "how we onboard people")})
	store, err := memory.OpenLocal(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := c.AttachMemory(context.Background(), store); err != nil {
		t.Fatalf("AttachMemory: %v", err)
	}
	return c, store
}

// memoryDoc renders a memory document for key.
func memoryDoc(t *testing.T, title, body string) []byte {
	t.Helper()
	return []byte("---\ntitle: " + title + "\ntype: Memory\ncategory: memory\n---\n\n" + body + "\n")
}

func TestCollection_SavedMemoryIsImmediatelySearchable(t *testing.T) {
	ctx := context.Background()
	c, _ := memColl(t)

	// Build the index BEFORE the write, so this really is an
	// incremental update to a live index rather than a first build that
	// happened to include the memory.
	if _, err := c.Index(); err != nil {
		t.Fatalf("Index: %v", err)
	}
	hits, err := c.mustSearch(ctx, "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("found %d hits before the write", len(hits))
	}

	version, page, err := c.SaveMemory(ctx, "team/drain.md",
		memoryDoc(t, "Draining nodes", "Always cordon the kubernetes node before draining it."),
		memory.CreateOnly())
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if version == "" {
		t.Error("SaveMemory returned no version")
	}
	if page.ID != "memory/team/drain" {
		t.Errorf("page id = %q, want memory/team/drain", page.ID)
	}

	// The whole point: no restart, no rebuild.
	hits, err = c.mustSearch(ctx, "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != "memory/team/drain" {
		t.Fatalf("search after save = %+v, want the memory", hits)
	}

	// And through the other two read surfaces.
	got, err := c.Load("memory/team/drain")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Title != "Draining nodes" {
		t.Errorf("Load title = %q", got.Title)
	}
	pages, err := c.Pages()
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(pages, "memory/team/drain") {
		t.Error("Pages does not include the saved memory")
	}
	if !containsID(pages, "handbook/onboarding") {
		t.Error("Pages lost the content page when the overlay was merged")
	}
}

// mustSearch queries the collection's index directly.
func (c *Collection) mustSearch(ctx context.Context, q string) ([]struct {
	Page kb.Page
}, error) {
	idx, err := c.Index()
	if err != nil {
		return nil, err
	}
	results, err := idx.QueryContext(ctx, q, 10)
	if err != nil {
		return nil, err
	}
	out := make([]struct{ Page kb.Page }, 0, len(results))
	for _, r := range results {
		out = append(out, struct{ Page kb.Page }{Page: r.Page})
	}
	return out, nil
}

func containsID(pages []kb.Page, id string) bool {
	for _, p := range pages {
		if p.ID == id {
			return true
		}
	}
	return false
}

func TestCollection_ResavingAMemoryReplacesItInTheIndex(t *testing.T) {
	ctx := context.Background()
	c, _ := memColl(t)

	v1, _, err := c.SaveMemory(ctx, "team/note.md", memoryDoc(t, "Note", "the aardvark fact"), memory.CreateOnly())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.SaveMemory(ctx, "team/note.md", memoryDoc(t, "Note", "the buffalo fact"), memory.UpdateFrom(v1)); err != nil {
		t.Fatal(err)
	}

	hits, err := c.mustSearch(ctx, "buffalo")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("searching for the new body found %d hits, want 1", len(hits))
	}
	// The superseded body is gone: bleve replaces a document indexed
	// under an existing ID rather than adding a second one.
	hits, err = c.mustSearch(ctx, "aardvark")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("the superseded body is still indexed (%d hits)", len(hits))
	}
	// Exactly one page, not two.
	pages, err := c.Pages()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range pages {
		if strings.HasPrefix(p.ID, "memory/") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("collection holds %d memories, want 1", n)
	}
}

func TestCollection_ConflictLeavesTheIndexUntouched(t *testing.T) {
	ctx := context.Background()
	c, _ := memColl(t)

	if _, _, err := c.SaveMemory(ctx, "team/note.md", memoryDoc(t, "Note", "original elephant"), memory.CreateOnly()); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.SaveMemory(ctx, "team/note.md", memoryDoc(t, "Note", "clobbering giraffe"), memory.CreateOnly())
	if !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	hits, err := c.mustSearch(ctx, "giraffe")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Error("a refused write reached the search index")
	}
	if hits, err := c.mustSearch(ctx, "elephant"); err != nil || len(hits) != 1 {
		t.Errorf("the original memory was disturbed: %d hits, err=%v", len(hits), err)
	}
}

func TestCollection_AttachMemoryLoadsWhatAnEarlierProcessWrote(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "memory")

	// One "process" writes.
	first, err := memory.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Put(ctx, "personal/ns1/fact.md", memoryDoc(t, "A Fact", "the narwhal detail"), memory.CreateOnly()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Stage(ctx, memory.StagingPrefix+"/team/ns1/proposal.md", memoryDoc(t, "Proposal", "the unreviewed okapi claim")); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	// A later one mounts the same directory.
	second, err := memory.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	c := FromPages("notes", nil)
	if err := c.AttachMemory(ctx, second); err != nil {
		t.Fatalf("AttachMemory: %v", err)
	}

	if _, err := c.Load("memory/personal/ns1/fact"); err != nil {
		t.Errorf("a memory written by an earlier process is not readable: %v", err)
	}
	hits, err := c.mustSearch(ctx, "narwhal")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("a memory written by an earlier process is not searchable (%d hits)", len(hits))
	}

	// The staged proposal is NOT loaded, NOT listed and NOT searchable —
	// a restart must not promote a pending artifact.
	if hits, err := c.mustSearch(ctx, "okapi"); err != nil || len(hits) != 0 {
		t.Errorf("a staged memory became searchable after a restart (%d hits, err=%v)", len(hits), err)
	}
	pages, err := c.Pages()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		if strings.Contains(p.ID, memory.StagingPrefix) {
			t.Errorf("staged document %q is being served", p.ID)
		}
	}
}

func TestCollection_StageMemoryNeitherIndexesNorLists(t *testing.T) {
	ctx := context.Background()
	c, _ := memColl(t)

	loc, err := c.StageMemory(ctx, memory.StagingPrefix+"/global/ns1/p.md", memoryDoc(t, "Proposal", "the pending wombat claim"))
	if err != nil {
		t.Fatalf("StageMemory: %v", err)
	}
	if !strings.Contains(loc, memory.StagingPrefix) {
		t.Errorf("location = %q, want it under %s", loc, memory.StagingPrefix)
	}
	if hits, err := c.mustSearch(ctx, "wombat"); err != nil || len(hits) != 0 {
		t.Errorf("a staged memory is searchable (%d hits, err=%v)", len(hits), err)
	}
	if _, err := c.Load("memory/global/ns1/p"); err == nil {
		t.Error("a staged memory is showable")
	}
}

func TestCollection_NoMemoryStoreIsReadOnly(t *testing.T) {
	ctx := context.Background()
	c := FromPages("plain", []kb.Page{page("a/b", "B", "body")})
	if c.Memory() != nil {
		t.Fatal("a collection with no memory: block has a store")
	}
	if _, _, err := c.SaveMemory(ctx, "team/x.md", []byte("x"), memory.CreateOnly()); err == nil {
		t.Error("SaveMemory on a read-only collection succeeded")
	}
	if _, err := c.StageMemory(ctx, memory.StagingPrefix+"/team/ns/x.md", []byte("x")); err == nil {
		t.Error("StageMemory on a read-only collection succeeded")
	}
}

func TestRegistry_WithMemoryNarrowsToWritableCollections(t *testing.T) {
	writableCol, _ := memColl(t)
	readOnly := FromPages("archive", []kb.Page{page("x/y", "Y", "body")})
	reg, err := New(readOnly, writableCol)
	if err != nil {
		t.Fatal(err)
	}

	got := reg.MemoryNames()
	if len(got) != 1 || got[0] != "notes" {
		t.Errorf("MemoryNames = %v, want [notes]", got)
	}
	// The view borrows rather than owns, exactly as Restrict does, so
	// closing it must not tear down the real collections' indexes.
	view := reg.WithMemory()
	if err := view.Close(); err != nil {
		t.Fatalf("Close on a derived view: %v", err)
	}
	if _, err := writableCol.Index(); err != nil {
		t.Fatalf("the underlying collection's index was torn down: %v", err)
	}
	// Composing with Restrict is what the memory tool does.
	none := reg.Restrict(func(name string) bool { return name == "archive" }).WithMemory()
	if none.Len() != 0 {
		t.Errorf("a caller who can only reach the read-only collection sees %v", none.Names())
	}
}

func TestCollection_ConcurrentSavesAreRaceFree(t *testing.T) {
	ctx := context.Background()
	c, _ := memColl(t)

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "team/note-" + string(rune('a'+i)) + ".md"
			if _, _, err := c.SaveMemory(ctx, key, memoryDoc(t, "N", "body"), memory.CreateOnly()); err != nil {
				t.Errorf("SaveMemory(%s): %v", key, err)
			}
		}(i)
	}
	// Readers run alongside the writers: the registry shares one
	// *Collection across every session, so Pages/Load/Index must be safe
	// against a concurrent save.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := c.Pages(); err != nil {
					t.Errorf("Pages: %v", err)
					return
				}
				_, _ = c.Load("memory/team/note-a")
				if _, err := c.mustSearch(ctx, "body"); err != nil {
					t.Errorf("search: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	pages, err := c.Pages()
	if err != nil {
		t.Fatal(err)
	}
	saved := 0
	for _, p := range pages {
		if strings.HasPrefix(p.ID, "memory/team/note-") {
			saved++
		}
	}
	if saved != n {
		t.Errorf("collection holds %d memories, want %d", saved, n)
	}
}
