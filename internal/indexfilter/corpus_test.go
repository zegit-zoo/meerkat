// Package indexfilter holds the measurement harness for GitHub issue #1
// ("Assess index-time frontmatter filtering to support very large
// knowledge bases") — see docs/design/index-filtering.md for the design
// document these benchmarks produced the numbers for.
//
// Everything here lives behind _test.go so none of it is compiled into
// any production binary or executed by a plain `go test ./...` /
// `make test` (Go only runs Benchmark* bodies when -bench is passed):
// this is assessment tooling, not a shipped feature.
package indexfilter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// categories are the values a synthetic corpus's pages carry in their
// `category` frontmatter field, assigned round-robin (page i gets
// categories[i%len(categories)]) so that exactly 1/len(categories) of
// the corpus — 10%, with 10 categories — carries targetCategory. That
// 10% is what every "filtered" benchmark below selects on, matching the
// issue's "a filter matches ~10%" scenario.
var categories = []string{
	"ops", "platform", "product", "security", "data",
	"infra", "frontend", "backend", "docs", "legal",
}

// targetCategory is the ~10% slice every filtered-build benchmark keeps.
const targetCategory = "ops"

var statuses = []string{"draft", "reviewed", "placeholder", "deprecated"}
var owners = []string{"team-payments", "team-platform", "team-search", "team-growth", "team-infra"}
var tagPool = []string{"runbook", "architecture", "incident", "onboarding", "reference", "policy", "adr", "howto", "faq", "glossary"}

// wordBank seeds synthetic body text. Real wiki prose isn't lorem ipsum,
// but for measuring "how long does it take to open, read and parse N
// files of a few KB each" the actual words don't matter — only the byte
// count and the fact bleve has to tokenise/analyse real English-shaped
// text rather than one repeated string (which some analysers special-case).
var wordBank = strings.Fields(`the quick brown fox jumps over lazy dog service latency
retry queue database cache token schema deploy rollout canary metric alert
dashboard incident runbook owner escalation timeout backoff circuit breaker
idempotency shard replica leader follower consensus quorum partition offset
consumer producer topic broker cluster node pod container image registry
pipeline build artifact release rollback canary blue green feature flag
config secret credential rotation audit log trace span request response
latency p99 throughput capacity autoscale threshold budget error rate slo
sli objective indicator dependency upstream downstream client server proxy
gateway load balancer firewall network subnet region zone availability`)

// genBody deterministically generates markdown body text of at least
// minBytes, as a handful of paragraphs — "a few KB" per the issue.
func genBody(rnd *rand.Rand, minBytes int) string {
	var b strings.Builder
	for b.Len() < minBytes {
		sentenceLen := 8 + rnd.Intn(10)
		for i := 0; i < sentenceLen; i++ {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(wordBank[rnd.Intn(len(wordBank))])
		}
		b.WriteString(".\n")
		if rnd.Intn(4) == 0 {
			b.WriteByte('\n') // paragraph break
		}
	}
	return b.String()
}

// GenerateCorpus writes n synthetic markdown pages with realistic
// frontmatter (category/subcategory/owner/status/tags/language/extra)
// and a body of a few KB each, under root/wiki/ — the content-repo
// layout internal/kbdir.FS adapts onto the "content/..." paths
// internal/kb expects. Deterministic from seed, so repeated runs (and
// the different benchmark functions below) see byte-identical corpora.
func GenerateCorpus(root string, n int, seed int64) error {
	rnd := rand.New(rand.NewSource(seed)) // deterministic test fixture, not a security context
	for i := 0; i < n; i++ {
		cat := categories[i%len(categories)]
		sub := fmt.Sprintf("area-%d", i%4)
		owner := owners[i%len(owners)]
		status := statuses[i%len(statuses)]
		lang := "en"
		if i%17 == 0 {
			lang = "sv"
		}
		nTags := 1 + rnd.Intn(3)
		tags := make([]string, 0, nTags)
		for t := 0; t < nTags; t++ {
			tags = append(tags, tagPool[rnd.Intn(len(tagPool))])
		}
		id := fmt.Sprintf("%s/page-%05d", cat, i)
		title := fmt.Sprintf("%s page %05d", strings.ToTitle(cat[:1])+cat[1:], i)

		var fm bytes.Buffer
		fm.WriteString("---\n")
		fmt.Fprintf(&fm, "id: %s\n", id)
		fmt.Fprintf(&fm, "title: %q\n", title)
		fmt.Fprintf(&fm, "category: %s\n", cat)
		fmt.Fprintf(&fm, "subcategory: %s\n", sub)
		fmt.Fprintf(&fm, "owner: %s\n", owner)
		fmt.Fprintf(&fm, "status: %s\n", status)
		fmt.Fprintf(&fm, "language: %s\n", lang)
		fm.WriteString("tags: [" + strings.Join(tags, ", ") + "]\n")
		fm.WriteString("extra:\n")
		fmt.Fprintf(&fm, "  region: %s\n", []string{"us-east", "us-west", "eu-central"}[i%3])
		fmt.Fprintf(&fm, "  priority: %d\n", i%5)
		fm.WriteString("---\n")

		body := genBody(rnd, 2000+rnd.Intn(2000)) // 2-4 KB, per the issue's "a few KB"
		content := fm.String() + "\n# " + title + "\n\n" + body

		p := filepath.Join(root, "wiki", cat, fmt.Sprintf("page-%05d.md", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// quickFrontmatter reads only the YAML frontmatter block from an already
// -open file, stopping at the closing "---" delimiter WITHOUT reading
// the page body — approximating benchmark (c) from the issue ("a
// frontmatter-only parse pass that skips body parsing for non-matching
// pages"). It returns a reader positioned right after the delimiter so a
// caller that decides to keep the page can read the body from the same
// handle instead of reopening the file.
//
// This is a deliberately simplified reimplementation of
// internal/kb's unexported splitFrontmatter (no Extra-map population, no
// OKF reserved-artifact handling, no \r\n tolerance beyond TrimSpace) —
// adequate for a benchmark that only ever reads the category/status/
// owner/tags/language fields it itself generated, not a replacement for
// the production parser.
func quickFrontmatter(f fs.File) (fm kb.Frontmatter, rest *bufio.Reader, ok bool) {
	br := bufio.NewReaderSize(f, 4096)
	first, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(first) != "---" {
		return kb.Frontmatter{}, br, false
	}
	var buf bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if strings.TrimSpace(line) == "---" {
			break
		}
		if err != nil {
			return kb.Frontmatter{}, br, false // never closed
		}
		buf.WriteString(line)
	}
	if err := yaml.Unmarshal(buf.Bytes(), &fm); err != nil {
		return kb.Frontmatter{}, br, false
	}
	return fm, br, true
}

// idFromContentPath derives a page ID from a "content/..."-rooted path,
// mirroring internal/kb's unexported id derivation (path minus the
// "content/" prefix and ".md" suffix).
func idFromContentPath(p string) string {
	return strings.TrimSuffix(strings.TrimPrefix(p, "content/"), ".md")
}

// listFrontmatterFiltered walks fsys exactly like kb.ListFS, but opens
// each file once and, when match(fm) is false, reads only the
// frontmatter and never touches the body at all — benchmark (c).
func listFrontmatterFiltered(fsys fs.FS, match func(kb.Frontmatter) bool) ([]kb.Page, error) {
	var pages []kb.Page
	err := fs.WalkDir(fsys, "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		fm, rest, ok := quickFrontmatter(f)
		if !ok || !match(fm) {
			return nil // body never read for a non-match
		}
		bodyBytes, err := io.ReadAll(rest)
		if err != nil {
			return err
		}
		id := idFromContentPath(p)
		if fm.ID == "" {
			fm.ID = id
		}
		title := fm.Title
		if title == "" {
			title = id
		}
		pages = append(pages, kb.Page{ID: id, Path: p, Title: title, Body: string(bodyBytes), Front: fm})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pages, nil
}

// manifestEntry is the sidecar record benchmark (d) simulates reading at
// startup: just enough frontmatter to filter on, not the page itself.
type manifestEntry struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	Category    string   `json:"category"`
	Subcategory string   `json:"subcategory,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Status      string   `json:"status,omitempty"`
	Language    string   `json:"language,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// buildManifest performs the one-time, off-the-startup-path scan a
// manifest-backed design would run at ingest time (or on a periodic
// refresh): every file is opened once and its frontmatter parsed, same
// cost as listFrontmatterFiltered's non-matching path, but every page
// contributes an entry regardless of any filter (the manifest describes
// the whole corpus so any future filter can be answered from it without
// rescanning).
func buildManifest(fsys fs.FS) ([]byte, error) {
	var entries []manifestEntry
	err := fs.WalkDir(fsys, "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		fm, _, ok := quickFrontmatter(f)
		_ = f.Close()
		if !ok {
			return nil
		}
		id := idFromContentPath(p)
		entries = append(entries, manifestEntry{
			ID: id, Path: p, Category: fm.Category, Subcategory: fm.Subcategory,
			Owner: fm.Owner, Status: fm.Status, Language: fm.Language, Tags: fm.Tags,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(entries)
}

// loadFilteredViaManifest is the steady-state startup cost of a
// manifest-backed design (benchmark (d)): decode the pre-built manifest
// (cheap — no file opens at all) and Open() ONLY the matching paths. A
// non-matching file is never opened this run, unlike (b) and (c), which
// both still visit every file once.
func loadFilteredViaManifest(fsys fs.FS, manifestBytes []byte, match func(manifestEntry) bool) ([]kb.Page, error) {
	var entries []manifestEntry
	if err := json.Unmarshal(manifestBytes, &entries); err != nil {
		return nil, err
	}
	pages := make([]kb.Page, 0, len(entries)/8) // rough ~10% filter-match sizing hint, not exact
	for _, e := range entries {
		if !match(e) {
			continue
		}
		page, err := kb.LoadFS(fsys, e.ID)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	return pages, nil
}
