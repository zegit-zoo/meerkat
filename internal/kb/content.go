// Package kb embeds the knowledge-base wiki content at build time and
// exposes it through a small read-only filesystem API.
//
// Content is synced from the configured content source into
// internal/kb/content/ at build time (see internal/contentsource and
// content-source.yaml) and embedded verbatim. Files matching excludedFiles
// are filtered at access time so we never serve build artefacts.
//
// Each page may carry YAML frontmatter delimited by '---'. We parse
// it into Frontmatter and expose category / owner / status / source
// / tags / related as first-class fields the search index can boost
// and the CLI/MCP/HTTP layers can filter on.
package kb

import (
	"bufio"
	"embed"
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed all:content
var wikiFS embed.FS

var excludedFiles = []string{
	"content/lint-report.md",
}

// Source describes where a page was ingested from. Agents use this
// to fetch the canonical version via gh / glab / pdftotext / etc.
type Source struct {
	// Host is the git hosting provider: github | gitlab | local.
	// Empty is inferred at ingestion time (see internal/sources.Normalize):
	// "local" for synthesised/pdfs sources, "github" otherwise.
	Host          string `yaml:"host,omitempty"  json:"host,omitempty"`
	Type          string `yaml:"type,omitempty"  json:"type,omitempty"`
	Repo          string `yaml:"repo,omitempty"  json:"repo,omitempty"`
	Group         string `yaml:"group,omitempty" json:"group,omitempty"`
	Ref           string `yaml:"ref,omitempty"   json:"ref,omitempty"`
	Path          string `yaml:"path,omitempty"  json:"path,omitempty"`
	WebURL        string `yaml:"web_url,omitempty"        json:"web_url,omitempty"`
	Version       string `yaml:"version,omitempty"        json:"version,omitempty"`
	EffectiveDate string `yaml:"effective_date,omitempty" json:"effective_date,omitempty"`
}

// Frontmatter holds the common engine-core fields present in every
// meerkat page. Deployment-specific fields are captured in Extra so the
// engine core stays generic without losing data.
type Frontmatter struct {
	ID           string   `yaml:"id"             json:"id,omitempty"`
	Title        string   `yaml:"title"          json:"title,omitempty"`
	Category     string   `yaml:"category"       json:"category,omitempty"`
	Subcategory  string   `yaml:"subcategory"    json:"subcategory,omitempty"`
	Owner        string   `yaml:"owner"          json:"owner,omitempty"`
	Status       string   `yaml:"status"         json:"status,omitempty"`
	Tags         []string `yaml:"tags"           json:"tags,omitempty"`
	Related      []string `yaml:"related"        json:"related,omitempty"`
	Source       Source   `yaml:"source"         json:"source,omitempty"`
	LastIngested string   `yaml:"last_ingested"  json:"last_ingested,omitempty"`
	Language     string   `yaml:"language"       json:"language,omitempty"`
	// FailureReason is set by the ingestion pipeline when a page could
	// not be refreshed, so operators can triage failures.
	FailureReason string `yaml:"failure_reason" json:"failure_reason,omitempty"`
	// Extra captures deployment-specific frontmatter fields not in the
	// common core (e.g. regulator, tier, superseded_by). Values are
	// preserved as parsed YAML scalars or structures.
	Extra map[string]any `yaml:"extra,omitempty" json:"extra,omitempty"`
}

// Page represents a single wiki page with normalised metadata.
type Page struct {
	ID    string      `json:"id"`
	Path  string      `json:"path"`
	Title string      `json:"title"`
	Body  string      `json:"body"`
	Front Frontmatter `json:"front"`
}

// ErrNotFound is returned by Load when a page does not exist.
var ErrNotFound = errors.New("page not found")

// FS returns the underlying embedded filesystem.
func FS() fs.FS { return wikiFS }

// List returns all wiki pages, sorted by ID for deterministic output.
func List() ([]Page, error) {
	var pages []Page
	err := fs.WalkDir(wikiFS, "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		if isExcluded(p) {
			return nil
		}
		pg, err := loadByPath(p)
		if err != nil {
			return err
		}
		pages = append(pages, pg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].ID < pages[j].ID })
	return pages, nil
}

// Load returns the page for the given ID. ID is the slash-separated
// path from the wiki root without the .md suffix. Both leading slash
// and the .md suffix are tolerated.
func Load(id string) (Page, error) {
	id = normaliseID(id)
	p := path.Join("content", id+".md")
	if isExcluded(p) {
		return Page{}, ErrNotFound
	}
	return loadByPath(p)
}

// FilterFunc selects pages from a slice; nil keeps everything.
type FilterFunc func(Page) bool

// Filter returns pages where keep(p) is true.
func Filter(pages []Page, keep FilterFunc) []Page {
	if keep == nil {
		return pages
	}
	out := pages[:0:0]
	for _, p := range pages {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}

// ByCategory is a FilterFunc preset.
func ByCategory(cat string) FilterFunc {
	return func(p Page) bool { return p.Front.Category == cat }
}

// ByStatus is a FilterFunc preset.
func ByStatus(status string) FilterFunc {
	return func(p Page) bool { return p.Front.Status == status }
}

// ByOwner is a FilterFunc preset.
func ByOwner(owner string) FilterFunc {
	return func(p Page) bool { return p.Front.Owner == owner }
}

// ByPrefix matches pages whose ID starts with the given prefix.
func ByPrefix(prefix string) FilterFunc {
	if prefix == "" {
		return nil
	}
	return func(p Page) bool { return strings.HasPrefix(p.ID, prefix) }
}

func loadByPath(p string) (Page, error) {
	body, err := fs.ReadFile(wikiFS, p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Page{}, ErrNotFound
		}
		return Page{}, err
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, "content/"), ".md")
	front, bodyOnly := splitFrontmatter(string(body))
	if front.ID == "" {
		front.ID = id
	}
	title := front.Title
	if title == "" {
		title = extractTitle(bodyOnly, id)
	}
	return Page{
		ID:    id,
		Path:  p,
		Title: title,
		Body:  bodyOnly,
		Front: front,
	}, nil
}

// coreKeys is the set of YAML top-level keys that are part of the typed
// Frontmatter core. Any key NOT in this set ends up in Frontmatter.Extra.
var coreKeys = map[string]bool{
	"id":             true,
	"title":          true,
	"category":       true,
	"subcategory":    true,
	"owner":          true,
	"status":         true,
	"tags":           true,
	"related":        true,
	"source":         true,
	"last_ingested":  true,
	"language":       true,
	"failure_reason": true,
}

// splitFrontmatter returns parsed frontmatter plus body-without-
// frontmatter. If no frontmatter is present the input is returned
// as the body and Frontmatter{} is returned.
//
// Unknown top-level keys (those not in the engine core) are collected
// into Frontmatter.Extra so no data is lost.
func splitFrontmatter(content string) (Frontmatter, string) {
	var fm Frontmatter
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return fm, content
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var (
		yamlLines []string
		bodyStart int
		seenOpen  bool
		closed    bool
	)
	pos := 0
	for scanner.Scan() {
		line := scanner.Text()
		pos += len(line) + 1 // +1 for the newline
		if !seenOpen {
			if strings.TrimSpace(line) == "---" {
				seenOpen = true
				continue
			}
			break
		}
		if strings.TrimSpace(line) == "---" {
			closed = true
			bodyStart = pos
			break
		}
		yamlLines = append(yamlLines, line)
	}
	if !closed {
		return fm, content
	}
	raw := []byte(strings.Join(yamlLines, "\n"))
	if err := yaml.Unmarshal(raw, &fm); err != nil {
		// On parse error, treat as no frontmatter rather than fail
		// the whole page load. The lint task can flag invalid YAML.
		return Frontmatter{}, content
	}
	// Second pass: collect all top-level keys that are not in the core
	// into fm.Extra so deployment-specific fields are preserved.
	var allFields map[string]any
	if err := yaml.Unmarshal(raw, &allFields); err == nil {
		extra := make(map[string]any, len(allFields))
		for k, v := range allFields {
			if !coreKeys[k] {
				extra[k] = v
			}
		}
		if len(extra) > 0 {
			fm.Extra = extra
		}
	}
	if bodyStart > len(content) {
		bodyStart = len(content)
	}
	return fm, content[bodyStart:]
}

// extractTitle returns the first markdown heading in body, falling
// back to the page ID if no heading is present.
func extractTitle(body, fallback string) string {
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			t := strings.TrimSpace(strings.TrimLeft(trim, "#"))
			if t != "" {
				return t
			}
		}
	}
	return fallback
}

func isExcluded(p string) bool {
	for _, ex := range excludedFiles {
		if p == ex {
			return true
		}
	}
	return false
}

func normaliseID(id string) string {
	id = strings.TrimPrefix(id, "/")
	id = strings.TrimSuffix(id, ".md")
	return id
}

// MarshalFrontmatter renders a Frontmatter as a `---`-delimited
// YAML block including trailing newline. Used by ingestion tools
// to rewrite a page's metadata without touching the body.
func MarshalFrontmatter(fm Frontmatter) (string, error) {
	out, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}
	return "---\n" + string(out) + "---\n", nil
}
