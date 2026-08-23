package indexfilter

import (
	"fmt"
	"io/fs"
	"os"
	"sync"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// corpusSizes covers the issue's requested "1k / 10k / 50k pages".
var corpusSizes = []int{1000, 10000, 50000}

// corpusCache memoizes generated corpora by size across every benchmark
// function in this file (they all need the same three corpora), so a
// full `go test -bench=. ./internal/indexfilter/...` run generates each
// size once rather than once per approach. Cleaned up by TestMain.
var (
	corpusMu    sync.Mutex
	corpusCache = map[int]string{}
)

func corpusRoot(b *testing.B, n int) string {
	b.Helper()
	corpusMu.Lock()
	defer corpusMu.Unlock()
	if root, ok := corpusCache[n]; ok {
		return root
	}
	root, err := os.MkdirTemp("", fmt.Sprintf("indexfilter-corpus-%d-", n))
	if err != nil {
		b.Fatalf("MkdirTemp: %v", err)
	}
	if err := GenerateCorpus(root, n, 42); err != nil {
		b.Fatalf("GenerateCorpus(%d): %v", n, err)
	}
	corpusCache[n] = root
	return root
}

// TestMain lets the cached corpora (see corpusRoot) outlive any single
// benchmark function while still being removed once the whole test
// binary — i.e. one `go test -bench=.` invocation — finishes.
func TestMain(m *testing.M) {
	code := m.Run()
	corpusMu.Lock()
	for _, root := range corpusCache {
		_ = os.RemoveAll(root)
	}
	corpusMu.Unlock()
	os.Exit(code)
}

func corpusFS(b *testing.B, n int) fs.FS {
	b.Helper()
	fsys, err := kbdir.FS(corpusRoot(b, n))
	if err != nil {
		b.Fatalf("kbdir.FS: %v", err)
	}
	return fsys
}

// byCategory is the frontmatter-level equivalent of kb.ByCategory, for
// callers (listFrontmatterFiltered, loadFilteredViaManifest) that decide
// whether to keep a page before a kb.Page even exists.
func byCategory(cat string) func(kb.Frontmatter) bool {
	return func(fm kb.Frontmatter) bool { return fm.Category == cat }
}

// BenchmarkListOnly isolates kb.ListFS's own cost (open+read+parse every
// file fully, no bleve at all) from search.NewFromPages's — splitting
// BenchmarkFullBuild's total into "reading the corpus" vs "indexing it",
// which turns out to matter a great deal for where filtering should cut
// in (see docs/design/index-filtering.md).
func BenchmarkListOnly(b *testing.B) {
	for _, n := range corpusSizes {
		fsys := corpusFS(b, n)
		b.Run(fmt.Sprintf("pages=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				pages, err := kb.ListFS(fsys)
				if err != nil {
					b.Fatal(err)
				}
				if len(pages) == 0 {
					b.Fatal("expected pages, got none")
				}
			}
		})
	}
}

// BenchmarkFullBuild is (a): kb.ListFS + search.NewFromPages over every
// page — today's only option, and the baseline every other approach is
// measured against.
func BenchmarkFullBuild(b *testing.B) {
	for _, n := range corpusSizes {
		fsys := corpusFS(b, n)
		b.Run(fmt.Sprintf("pages=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				pages, err := kb.ListFS(fsys)
				if err != nil {
					b.Fatal(err)
				}
				idx, err := search.NewFromPages(pages)
				if err != nil {
					b.Fatal(err)
				}
				_ = idx.Close()
			}
		})
	}
}

// BenchmarkFilteredPostList is (b): kb.ListFS over every page (full
// parse, same as (a)), THEN kb.Filter narrows to the ~10% match before
// indexing. Isolates whether filtering AFTER the fact — the simplest
// possible "index-time filter" — actually saves the cost that matters,
// or only shrinks the resulting index.
func BenchmarkFilteredPostList(b *testing.B) {
	for _, n := range corpusSizes {
		fsys := corpusFS(b, n)
		b.Run(fmt.Sprintf("pages=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				pages, err := kb.ListFS(fsys)
				if err != nil {
					b.Fatal(err)
				}
				filtered := kb.Filter(pages, kb.ByCategory(targetCategory))
				idx, err := search.NewFromPages(filtered)
				if err != nil {
					b.Fatal(err)
				}
				_ = idx.Close()
			}
		})
	}
}

// BenchmarkFrontmatterOnlyFiltered is (c): every file is opened once, but
// the body is only read (and only a kb.Page built) for the ~10% that
// match — the rest cost one file open + a few hundred bytes of YAML
// parse, never a multi-KB body read.
func BenchmarkFrontmatterOnlyFiltered(b *testing.B) {
	for _, n := range corpusSizes {
		fsys := corpusFS(b, n)
		b.Run(fmt.Sprintf("pages=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				pages, err := listFrontmatterFiltered(fsys, byCategory(targetCategory))
				if err != nil {
					b.Fatal(err)
				}
				idx, err := search.NewFromPages(pages)
				if err != nil {
					b.Fatal(err)
				}
				_ = idx.Close()
			}
		})
	}
}

// BenchmarkManifestFiltered is (d)'s steady state: a manifest already
// exists (built once, off the timed path — see BenchmarkManifestBuild),
// so a "startup" only has to decode it (cheap) and open the ~10% of
// files that match. The other 90% of files are never opened at all this
// run, unlike (b) and (c).
func BenchmarkManifestFiltered(b *testing.B) {
	for _, n := range corpusSizes {
		fsys := corpusFS(b, n)
		manifestBytes, err := buildManifest(fsys)
		if err != nil {
			b.Fatalf("buildManifest(%d): %v", n, err)
		}
		b.Run(fmt.Sprintf("pages=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				pages, err := loadFilteredViaManifest(fsys, manifestBytes, func(e manifestEntry) bool {
					return e.Category == targetCategory
				})
				if err != nil {
					b.Fatal(err)
				}
				idx, err := search.NewFromPages(pages)
				if err != nil {
					b.Fatal(err)
				}
				_ = idx.Close()
			}
		})
	}
}

// BenchmarkManifestBuild measures the cost BenchmarkManifestFiltered
// deliberately excludes: building/refreshing the manifest itself. A
// real deployment pays this once at ingest time (or on a periodic
// refresh), never on the request/startup path — reported separately so
// the doc can show it's the same order of cost as (c)'s full scan, just
// moved off the hot path.
func BenchmarkManifestBuild(b *testing.B) {
	for _, n := range corpusSizes {
		fsys := corpusFS(b, n)
		b.Run(fmt.Sprintf("pages=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := buildManifest(fsys); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
