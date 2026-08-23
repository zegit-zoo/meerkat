package search

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// put_test.go covers incremental indexing: the mechanism that makes a
// memory saved by mk_save_memory searchable without a rebuild or a
// restart.

func TestPut_MakesAPageSearchableImmediately(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("concepts/eviction", "Cache eviction", "The cache evicts the least recently used entry.", "concepts"),
	})

	before, err := idx.Query("capybara", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("found %d hits before the write", len(before))
	}

	page := fixturePage("memory/team/fauna", "Fauna", "The capybara is the office mascot.", "memory")
	if err := idx.Put(page); err != nil {
		t.Fatalf("Put: %v", err)
	}

	after, err := idx.Query("capybara", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Page.ID != page.ID {
		t.Fatalf("after Put: %+v, want the new page", after)
	}
	// The page map is updated too, so a hit resolves to real content
	// rather than being dropped as index/page drift.
	if after[0].Page.Title != "Fauna" || !strings.Contains(after[0].Page.Body, "office mascot") {
		t.Errorf("hit carries the wrong page: %+v", after[0].Page)
	}
	// The pre-existing pages are untouched.
	if got, err := idx.Query("eviction", 10); err != nil || len(got) != 1 {
		t.Errorf("the original page was disturbed: %d hits, err=%v", len(got), err)
	}
}

func TestPut_ReplacesRatherThanDuplicates(t *testing.T) {
	idx := newTestIndex(t, nil)
	id := "memory/team/note"

	if err := idx.Put(fixturePage(id, "Note", "the aardvark fact", "memory")); err != nil {
		t.Fatal(err)
	}
	if err := idx.Put(fixturePage(id, "Note", "the buffalo fact", "memory")); err != nil {
		t.Fatal(err)
	}

	if got, err := idx.Query("buffalo", 10); err != nil || len(got) != 1 {
		t.Errorf("new body: %d hits, err=%v, want 1", len(got), err)
	}
	if got, err := idx.Query("aardvark", 10); err != nil || len(got) != 0 {
		t.Errorf("superseded body is still indexed: %d hits, err=%v", len(got), err)
	}
}

func TestPut_RefusesAPageWithNoID(t *testing.T) {
	idx := newTestIndex(t, nil)
	if err := idx.Put(kb.Page{Title: "No id"}); err == nil {
		t.Error("Put with no page ID succeeded")
	}
}

func TestPut_IsSafeAlongsideConcurrentQueries(t *testing.T) {
	// The registry shares one *Index across every MCP session, so a save
	// happens while other sessions are searching. The page map beside
	// bleve's index is the part that needed a lock.
	idx := newTestIndex(t, []kb.Page{
		fixturePage("concepts/eviction", "Cache eviction", "The cache evicts entries.", "concepts"),
	})

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 10 {
				p := fixturePage(fmt.Sprintf("memory/team/n-%d-%d", i, j), "Note", "shared body text", "memory")
				if err := idx.Put(p); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}(i)
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := idx.QueryContext(context.Background(), "shared body", 10); err != nil {
					t.Errorf("Query: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := idx.Query("shared", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 80 {
		t.Errorf("found %d pages, want 80", len(got))
	}
}
