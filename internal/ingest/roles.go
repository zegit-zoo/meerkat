package ingest

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// roles.go is the intake side of `mk ingest` (meerkat-mob issue H): the
// researcher and validator roles that turn a raw intake item into a
// machine-confirmed page, and the plumbing that keeps the loop
// idempotent. The librarian role is librarian.go.
//
// The executor is unchanged — an agent CLI run in the content working
// copy, told what to do by an instruction — so a mongoose skill or any
// other executor can replace it later. What changes per role is the
// instruction and the success check.

// Role is who an ingest run acts as.
type Role string

const (
	// RoleResearcher turns a raw intake item into a candidate page.
	RoleResearcher Role = "researcher"
	// RoleValidator re-derives a candidate's claims independently.
	RoleValidator Role = "validator"
	// RoleLibrarian reports on and repairs the tree. See librarian.go.
	RoleLibrarian Role = "librarian"
)

// ParseRole validates a --role value.
func ParseRole(s string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleResearcher:
		return RoleResearcher, nil
	case RoleValidator:
		return RoleValidator, nil
	case RoleLibrarian:
		return RoleLibrarian, nil
	case "":
		return "", nil
	}
	return "", fmt.Errorf("role must be %s, %s or %s, got %q", RoleResearcher, RoleValidator, RoleLibrarian, s)
}

//go:embed prompts/*.md
var defaultPrompts embed.FS

// RolePrompt returns the prompt for a role: the content repo's
// ingestion/prompts/<role>.md when it has one, else the built-in.
func RolePrompt(role Role) (string, error) {
	if p, err := sources.Prompt(string(role) + ".md"); err == nil && strings.TrimSpace(p) != "" {
		return p, nil
	}
	b, err := defaultPrompts.ReadFile("prompts/" + string(role) + ".md")
	if err != nil {
		return "", fmt.Errorf("no prompt for role %q: %w", role, err)
	}
	return string(b), nil
}

// Layout within the content working copy.
const (
	// IntakeDir is where raw items are materialised for the agent.
	IntakeDir = "ingestion/intake"
	// CandidateDir (under the wiki dir) is where candidate pages live
	// until the contract files them.
	CandidateDir = "intake"
	// AgentVerifierPrefix is what a validator's verified entry starts with.
	AgentVerifierPrefix = "agent:"
	// ConfirmationsRequired is how many independent agent verifiers
	// make a candidate machine-confirmed for filing (Q7).
	ConfirmationsRequired = 2
	// NeedsHumanPrefix in a failure_reason parks the item.
	NeedsHumanPrefix = "needs-human"
)

// IntakePlanOpts plans an intake run.
type IntakePlanOpts struct {
	Role Role
	// Namespace restricts a researcher to one identity's deposits; ""
	// is every namespace (a librarian's or an operator's view).
	Namespace string
	// Only restricts to one intake id.
	Only string
	// Workdir is the content working copy; raw items are materialised
	// under it.
	Workdir      string
	WikiDir      string
	Model        string
	SubagentType string
	WallClockCap int
	Now          func() time.Time
}

// Skip records why an item was not planned.
type Skip struct {
	IntakeID string
	Reason   string
}

// PlanIntake plans researcher or validator tasks from the intake store.
func PlanIntake(ctx context.Context, store *intake.Store, opts IntakePlanOpts) ([]Task, []Skip, error) {
	if store == nil {
		return nil, nil, errors.New("no intake store configured (content-source.yaml intake:)")
	}
	if opts.Workdir == "" {
		return nil, nil, errors.New("a content working copy is required (--workdir-kb)")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.WikiDir == "" {
		opts.WikiDir = DefaultWikiDir
	}
	if opts.Model == "" {
		opts.Model = DefaultModel
	}
	if opts.SubagentType == "" {
		opts.SubagentType = DefaultSubagentType
	}
	if opts.WallClockCap <= 0 {
		opts.WallClockCap = DefaultWallClockCap
	}
	prompt, err := RolePrompt(opts.Role)
	if err != nil {
		return nil, nil, err
	}
	switch opts.Role {
	case RoleResearcher:
		return planResearch(ctx, store, opts, prompt)
	case RoleValidator:
		return planValidation(ctx, store, opts, prompt)
	}
	return nil, nil, fmt.Errorf("role %q does not plan tasks", opts.Role)
}

func planResearch(ctx context.Context, store *intake.Store, opts IntakePlanOpts, prompt string) ([]Task, []Skip, error) {
	items, err := store.ListRaw(ctx, opts.Namespace, false)
	if err != nil {
		return nil, nil, err
	}
	var tasks []Task
	var skips []Skip
	for _, it := range items {
		if opts.Only != "" && it.ID != opts.Only {
			continue
		}
		if it.FallbackKind == "" || it.FallbackKind == "none" || strings.TrimSpace(it.Body) == "" {
			skips = append(skips, Skip{it.ID, "no research in the deposit (fallback none or empty body)"})
			continue
		}
		rawRel := path.Join(IntakeDir, it.ID+".md")
		if err := writeWithin(opts.Workdir, rawRel, []byte(it.Body)); err != nil {
			return nil, nil, err
		}
		pageID := path.Join(CandidateDir, it.ID)
		pageRel := pagePathForRepo(opts.WikiDir, pageID)
		subs := map[string]string{
			"raw_path":  rawRel,
			"intake_id": it.ID,
			"question":  it.Question,
			"attempted": strings.Join(it.Attempted, ", "),
			"sources":   strings.Join(it.Sources, ", "),
			"page_path": pageRel,
			"page_id":   pageID,
			"model":     opts.Model,
			"now":       opts.Now().UTC().Format(time.RFC3339),
			"target_kb": targetKB(it),
		}
		tasks = append(tasks, Task{
			Role: RoleResearcher, IntakeID: it.ID, RawPath: rawRel, TargetKB: targetKB(it),
			PageID: pageID, PagePath: pageRel, SourceID: "intake",
			Prompt: substitute(prompt, subs), Model: opts.Model, SubagentType: opts.SubagentType, WallClockCapS: opts.WallClockCap,
		})
	}
	return tasks, skips, nil
}

// targetKB is where a candidate belongs: the deepest collection the
// agent tried (the last one), or "unrouted" for the librarian to place.
func targetKB(it intake.Item) string {
	if n := len(it.Attempted); n > 0 {
		last := it.Attempted[n-1]
		if i := strings.LastIndex(last, "/"); i >= 0 {
			last = last[i+1:]
		}
		return last
	}
	return "unrouted"
}

func planValidation(ctx context.Context, store *intake.Store, opts IntakePlanOpts, prompt string) ([]Task, []Skip, error) {
	staged, err := store.ListStaged(ctx)
	if err != nil {
		return nil, nil, err
	}
	var tasks []Task
	var skips []Skip
	for _, s := range staged {
		if opts.Only != "" && s.ID != opts.Only {
			continue
		}
		if done, _ := store.IsDone(ctx, "validated-"+s.ID); done {
			skips = append(skips, Skip{s.ID, "already confirmed"})
			continue
		}
		pageID := path.Join(CandidateDir, s.ID)
		pageRel := pagePathForRepo(opts.WikiDir, pageID)
		full := filepath.Join(opts.Workdir, filepath.FromSlash(pageRel))
		if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
			if err := writeWithin(opts.Workdir, pageRel, s.Body); err != nil {
				return nil, nil, err
			}
		}
		page, err := kb.ParsePage(pageID, pageRel, mustRead(full))
		if err != nil {
			skips = append(skips, Skip{s.ID, "candidate does not parse: " + err.Error()})
			continue
		}
		if rm, _ := page.Front.Extra["researcher_model"].(string); rm != "" && rm == opts.Model {
			skips = append(skips, Skip{s.ID, "validator must not run the researcher's model (" + rm + "); pass a different --model"})
			continue
		}
		if len(page.Front.Verified) >= ConfirmationsRequired {
			skips = append(skips, Skip{s.ID, "already has the required confirmations"})
			continue
		}
		subs := map[string]string{
			"page_path": pageRel, "intake_id": s.ID, "model": opts.Model,
			"now": opts.Now().UTC().Format(time.RFC3339),
		}
		tasks = append(tasks, Task{
			Role: RoleValidator, IntakeID: s.ID, TargetKB: s.KB,
			PageID: pageID, PagePath: pageRel, SourceID: "intake",
			Prompt: substitute(prompt, subs), Model: opts.Model, SubagentType: opts.SubagentType, WallClockCapS: opts.WallClockCap,
		})
	}
	return tasks, skips, nil
}

func substitute(tpl string, subs map[string]string) string {
	out := tpl
	for k, v := range subs {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p) //nolint:gosec // G304: a path inside the operator's working copy.
	return b
}

// writeWithin writes rel under base, refusing escapes, creating dirs.
func writeWithin(base, rel string, body []byte) error {
	full := filepath.Join(base, filepath.FromSlash(rel))
	if !isPathWithinBase(base, full) {
		return fmt.Errorf("path %q escapes the working copy", rel)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	return os.WriteFile(full, body, 0o600)
}

// Finalization is what Finalize did per task.
type Finalization struct {
	IntakeID string
	Action   string // staged | confirmed | failed | parked | pending | skipped
	Detail   string
}

// Finalize records the outcome of executed intake tasks in the intake
// store: a researcher's page is stamped and staged; a validator's
// confirmation or failure moves the item on, parks it after repeated
// failure, and marks it done once it has the required confirmations.
func Finalize(ctx context.Context, store *intake.Store, workdir string, results []Result, now time.Time) ([]Finalization, error) {
	var out []Finalization
	for _, r := range results {
		t := r.Task
		if t.Role == "" {
			continue
		}
		f := Finalization{IntakeID: t.IntakeID}
		if r.ExitStatus != "ok" {
			f.Action, f.Detail = "failed", r.FailReason
			telemetry.Record(ctx).IntakeItem(intake.StageStaged, intake.OutcomeFailed)
			out = append(out, f)
			continue
		}
		full := filepath.Join(workdir, filepath.FromSlash(t.PagePath))
		if !isPathWithinBase(workdir, full) {
			f.Action, f.Detail = "failed", "candidate path escapes the working copy"
			out = append(out, f)
			continue
		}
		body, err := os.ReadFile(full) //nolint:gosec // G304: checked against the working copy above.
		if err != nil {
			f.Action, f.Detail = "failed", "candidate page missing after run: "+err.Error()
			out = append(out, f)
			continue
		}
		page, err := kb.ParsePage(t.PageID, t.PagePath, body)
		if err != nil {
			f.Action, f.Detail = "failed", "candidate does not parse: "+err.Error()
			out = append(out, f)
			continue
		}
		switch t.Role {
		case RoleResearcher:
			stamped, changed := stampCandidate(page, body, t, now)
			if changed {
				if err := os.WriteFile(full, stamped, 0o600); err != nil { //nolint:gosec // G703: checked against the working copy above.
					return out, err
				}
			}
			key, err := store.PutStaged(ctx, t.TargetKB, t.IntakeID, stamped)
			if err != nil {
				return out, err
			}
			if err := store.MarkDone(ctx, t.IntakeID, "staged as "+key); err != nil {
				return out, err
			}
			f.Action, f.Detail = "staged", key
		case RoleValidator:
			switch {
			case strings.HasPrefix(strings.ToLower(page.Front.FailureReason), NeedsHumanPrefix):
				if err := store.Park(ctx, t.IntakeID, page.Front.FailureReason); err != nil {
					return out, err
				}
				telemetry.Record(ctx).LibrarianFinding("needs_human")
				f.Action, f.Detail = "parked", page.Front.FailureReason
			case page.Front.FailureReason != "":
				n := failureCount(page) + 1
				if n >= ConfirmationsRequired {
					reason := fmt.Sprintf("validators disagreed %d times; last: %s", n, page.Front.FailureReason)
					if err := store.Park(ctx, t.IntakeID, reason); err != nil {
						return out, err
					}
					telemetry.Record(ctx).LibrarianFinding("needs_human")
					f.Action, f.Detail = "parked", reason
				} else {
					f.Action, f.Detail = "pending", fmt.Sprintf("validation failed (%d of %d): %s", n, ConfirmationsRequired, page.Front.FailureReason)
				}
				_ = os.WriteFile(full, bumpFailureCount(page, n), 0o600) //nolint:gosec // G703: checked against the working copy above.
			case agentConfirmations(page) >= ConfirmationsRequired:
				if err := store.MarkDone(ctx, "validated-"+t.IntakeID, "confirmed by "+strings.Join(verifierNames(page), ", ")); err != nil {
					return out, err
				}
				f.Action, f.Detail = "confirmed", fmt.Sprintf("%d independent confirmations; ready to file into %q", agentConfirmations(page), t.TargetKB)
			default:
				f.Action, f.Detail = "pending", fmt.Sprintf("%d of %d confirmations", agentConfirmations(page), ConfirmationsRequired)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// agentConfirmations counts distinct agent verifiers.
func agentConfirmations(p kb.Page) int {
	seen := map[string]bool{}
	for _, v := range p.Front.Verified {
		if strings.HasPrefix(v.By, AgentVerifierPrefix) {
			seen[v.By] = true
		}
	}
	return len(seen)
}

func verifierNames(p kb.Page) []string {
	out := make([]string, 0, len(p.Front.Verified))
	for _, v := range p.Front.Verified {
		out = append(out, v.By)
	}
	return out
}

func failureCount(p kb.Page) int {
	if n, ok := p.Front.Extra["validation_failures"].(int); ok {
		return n
	}
	if f, ok := p.Front.Extra["validation_failures"].(float64); ok {
		return int(f)
	}
	return 0
}

// Confirmed reports whether a candidate page has the confirmations
// the pipeline requires to file it.
func Confirmed(p kb.Page) bool { return agentConfirmations(p) >= ConfirmationsRequired }

// stampCandidate fills the provenance a researcher may have left out:
// generated, last_ingested, extra.intake_id, extra.researcher_model,
// status unverified, verified []. It returns the page bytes and whether
// they changed.
func stampCandidate(p kb.Page, body []byte, t Task, now time.Time) ([]byte, bool) {
	changed := false
	if p.Front.Generated == nil {
		p.Front.Generated = &kb.Generated{By: "agent:" + string(RoleResearcher), At: now.UTC().Format(time.RFC3339)}
		changed = true
	}
	if p.Front.LastIngested == "" {
		p.Front.LastIngested = now.UTC().Format(time.RFC3339)
		changed = true
	}
	if p.Front.Status == "" || p.Front.Status == "placeholder" {
		p.Front.Status = "unverified"
		changed = true
	}
	if p.Front.Extra == nil {
		p.Front.Extra = map[string]any{}
	}
	if p.Front.Extra["intake_id"] != t.IntakeID {
		p.Front.Extra["intake_id"] = t.IntakeID
		changed = true
	}
	if p.Front.Extra["researcher_model"] != t.Model {
		p.Front.Extra["researcher_model"] = t.Model
		changed = true
	}
	if p.Front.Extra["target_kb"] != t.TargetKB {
		p.Front.Extra["target_kb"] = t.TargetKB
		changed = true
	}
	if !changed {
		return body, false
	}
	return renderPage(p), true
}

func bumpFailureCount(p kb.Page, n int) []byte {
	if p.Front.Extra == nil {
		p.Front.Extra = map[string]any{}
	}
	p.Front.Extra["validation_failures"] = n
	return renderPage(p)
}

// renderPage writes frontmatter + body. The parser collects every
// unknown TOP-LEVEL key into Frontmatter.Extra, so Extra is written back
// as top-level keys — never as a nested extra: block, which would come
// back as Extra["extra"].
func renderPage(p kb.Page) []byte {
	extra := p.Front.Extra
	p.Front.Extra = nil
	fm, err := yaml.Marshal(p.Front)
	if err != nil {
		return []byte(p.Body)
	}
	out := string(fm)
	if len(extra) > 0 {
		keys := make([]string, 0, len(extra))
		for k := range extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(map[string]any, len(extra))
		for _, k := range keys {
			ordered[k] = extra[k]
		}
		if ex, err := yaml.Marshal(ordered); err == nil {
			out += string(ex)
		}
	}
	return []byte("---\n" + out + "---\n" + strings.TrimLeft(p.Body, "\n"))
}
