// Package ingest implements the Meerkat ingestion planner +
// executor split.
//
// The planner walks the embedded KB pages, filters them down to
// the work to do (by source / status / explicit page id), renders
// the per-source-type prompt with mustache-style variables, and
// emits one JSONL Task per page. The executor spawns one
// `opencode run` per Task.
//
// The Go binary holds zero LLM creds — opencode handles that.
package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

// Task is one ingestion unit consumed by an Executor.
//
// Stable JSON shape — both `mk ingest --execute` and the
// `meerkat-ingest` OpenCode skill consume this format.
type Task struct {
	PageID        string         `json:"page_id"`
	PagePath      string         `json:"page_path"` // path within the content working copy, e.g. "wiki/policies/foo.md"
	SourceID      string         `json:"source_id"` // sources.yaml id
	Source        sources.Source `json:"source"`
	Prompt        string         `json:"prompt"`                 // fully rendered system prompt
	Model         string         `json:"model"`                  // e.g. "openai/gpt-5.5-fast"
	SubagentType  string         `json:"subagent_type"`          // e.g. "general"
	WallClockCapS int            `json:"wall_clock_cap_seconds"` // per-task hard cap
}

// PlanOpts narrows what the planner emits.
type PlanOpts struct {
	// SourceID restricts to a single source; "" means all sources.
	SourceID string
	// PageID restricts to a single page; takes precedence over the
	// other filters. Format: "wiki/policies/foo.md" OR a frontmatter
	// id like "policies/foo".
	PageID string
	// Statuses limits to pages whose frontmatter status is one of
	// these. Empty means {"placeholder", "ingest-failed"} (the
	// default "what needs work" set).
	Statuses []string
	// Model overrides the per-source model; empty defers to source
	// or DefaultModel.
	Model string
	// SubagentType overrides the OpenCode sub-agent type; empty
	// uses DefaultSubagentType.
	SubagentType string
	// WallClockCap overrides the per-task wall-clock cap in
	// seconds; 0 uses DefaultWallClockCap.
	WallClockCap int
	// Reverse flips the task order. Useful when running a second
	// parallel batch from the tail of the queue against an existing
	// forward batch — collisions are cheap because the executor
	// skips pages already at status: reviewed.
	Reverse bool
	// WikiDir is the content repo's wiki subdirectory (from
	// content-source.yaml layout.wiki); it prefixes Task.PagePath so
	// sub-agents write to the right place. Empty uses DefaultWikiDir.
	WikiDir string
}

// Defaults applied when fields are zero.
const (
	DefaultModel        = "openai/gpt-5.5-fast"
	DefaultSubagentType = "general"
	DefaultWallClockCap = 300
	DefaultWikiDir      = "wiki"
)

func defaultStatuses() []string {
	return []string{"placeholder", "ingest-failed"}
}

// Plan returns the ordered list of Tasks to execute.
//
// It does NOT touch disk beyond reading the embedded KB. Callers
// can serialise the result to JSONL with WriteJSONL.
func Plan(opts PlanOpts) ([]Task, error) {
	pages, err := kb.List()
	if err != nil {
		return nil, fmt.Errorf("list kb pages: %w", err)
	}

	// Resolve filters.
	if opts.PageID != "" {
		// Tolerate "wiki/policies/foo.md", "policies/foo.md", or
		// "policies/foo".
		want := normalisePageID(opts.PageID)
		filtered := make([]kb.Page, 0, 1)
		for _, p := range pages {
			if p.ID == want {
				filtered = append(filtered, p)
				break
			}
		}
		pages = filtered
	} else {
		statuses := opts.Statuses
		if len(statuses) == 0 {
			statuses = defaultStatuses()
		}
		statusSet := make(map[string]bool, len(statuses))
		for _, s := range statuses {
			statusSet[s] = true
		}
		filtered := pages[:0:0]
		for _, p := range pages {
			if !statusSet[p.Front.Status] {
				continue
			}
			filtered = append(filtered, p)
		}
		pages = filtered
	}

	// Load source registry once.
	allSources, err := sources.All()
	if err != nil {
		return nil, err
	}
	srcByCategory := make(map[string]sources.Source, len(allSources))
	srcByID := make(map[string]sources.Source, len(allSources))
	for _, s := range allSources {
		srcByCategory[s.TargetCategory] = s
		srcByID[s.ID] = s
	}

	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	subagent := opts.SubagentType
	if subagent == "" {
		subagent = DefaultSubagentType
	}
	cap := opts.WallClockCap
	if cap == 0 {
		cap = DefaultWallClockCap
	}
	wikiDir := opts.WikiDir
	if wikiDir == "" {
		wikiDir = DefaultWikiDir
	}

	tasks := make([]Task, 0, len(pages))
	for _, p := range pages {
		src, ok := matchSource(p, srcByCategory)
		if !ok {
			// No source registered for this page's category —
			// skip silently. (Could surface as a warning if we
			// add one.)
			continue
		}
		if opts.SourceID != "" && src.ID != opts.SourceID {
			continue
		}
		prompt, err := renderPrompt(p, src, wikiDir)
		if err != nil {
			return nil, fmt.Errorf("render prompt for %s: %w", p.ID, err)
		}
		taskModel := model
		if src.Model != "" && opts.Model == "" {
			taskModel = src.Model
		}
		tasks = append(tasks, Task{
			PageID:        p.ID,
			PagePath:      pagePathForRepo(wikiDir, p.ID),
			SourceID:      src.ID,
			Source:        src,
			Prompt:        prompt,
			Model:         taskModel,
			SubagentType:  subagent,
			WallClockCapS: cap,
		})
	}

	if opts.Reverse {
		for i, j := 0, len(tasks)-1; i < j; i, j = i+1, j-1 {
			tasks[i], tasks[j] = tasks[j], tasks[i]
		}
	}
	return tasks, nil
}

// WriteJSONL serialises tasks as one Task per line.
func WriteJSONL(w io.Writer, tasks []Task) error {
	enc := json.NewEncoder(w)
	for _, t := range tasks {
		if err := enc.Encode(t); err != nil {
			return err
		}
	}
	return nil
}

// renderPrompt loads the Source's prompt template and substitutes
// the {{var}} placeholders the prompts reference. Unknown markers
// are left as-is so prompt authors can spot them at runtime.
func renderPrompt(p kb.Page, src sources.Source, wikiDir string) (string, error) {
	tpl, err := sources.Prompt(src.Prompt)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	subs := map[string]string{
		"page_id":          p.ID,
		"page_path":        pagePathForRepo(wikiDir, p.ID),
		"page_id_basename": idBasename(p.ID),
		"template_path":    "templates/" + src.Template,
		"source":           formatSource(src),
		"enrichment":       strings.Join(src.Enrichment, ","),
		"now":              now,
	}
	out := tpl
	for k, v := range subs {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out, nil
}

// pagePathForRepo returns the path *as the content working copy sees it*,
// e.g. "wiki/policies/foo.md", where wikiDir is the repo's configured wiki
// subdirectory (content-source.yaml layout.wiki). The embedded path inside
// the binary is "content/policies/foo.md", which is not useful for sub-agents
// writing back to disk.
func pagePathForRepo(wikiDir, id string) string {
	return path.Join(wikiDir, id+".md")
}

func idBasename(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

func normalisePageID(in string) string {
	in = strings.TrimSuffix(in, ".md")
	in = strings.TrimPrefix(in, "wiki/")
	in = strings.TrimPrefix(in, "content/")
	return in
}

// matchSource picks the source for a page by target_category. Resolution
// order: category/subcategory, then category, then the longest target_category
// that is a path-prefix of the page id (a generic rule covering any nested
// taxonomy). Returns ok=false if no source covers the page.
func matchSource(p kb.Page, byCategory map[string]sources.Source) (sources.Source, bool) {
	// 1) exact category/subcategory hit, e.g. "operations/runbooks".
	if p.Front.Category != "" && p.Front.Subcategory != "" {
		if s, ok := byCategory[p.Front.Category+"/"+p.Front.Subcategory]; ok {
			return s, true
		}
	}
	// 2) plain category hit, e.g. "policies", "adr", "concepts".
	if p.Front.Category != "" {
		if s, ok := byCategory[p.Front.Category]; ok {
			return s, true
		}
	}
	// 3) longest target_category that is a path-prefix of the page id, e.g.
	//    page "systems/backend/foo" -> target_category "systems/backend".
	best := ""
	var bestSrc sources.Source
	for tc, s := range byCategory {
		if p.ID == tc || strings.HasPrefix(p.ID, tc+"/") {
			if len(tc) > len(best) {
				best, bestSrc = tc, s
			}
		}
	}
	if best != "" {
		return bestSrc, true
	}
	return sources.Source{}, false
}

// formatSource returns a one-line representation of the source for
// embedding in prompts.
func formatSource(s sources.Source) string {
	var b strings.Builder
	b.WriteString("type=" + s.Type)
	if s.Host != "" {
		fmt.Fprintf(&b, " host=%s", s.Host)
	}
	if s.Repo != "" {
		fmt.Fprintf(&b, " repo=%s", s.Repo)
	}
	if s.Group != "" {
		fmt.Fprintf(&b, " group=%s", s.Group)
	}
	if s.Path != "" {
		fmt.Fprintf(&b, " path=%s", s.Path)
	}
	if len(s.Enrichment) > 0 {
		fmt.Fprintf(&b, " enrichment=%s", strings.Join(s.Enrichment, ","))
	}
	return b.String()
}
