package ingest

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	root, err := os.OpenRoot(opts.Workdir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
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
		// A deposit that carries a credential is not researched: the
		// agent would copy it into a page that is committed and pushed.
		if rule := findSecret([]byte(it.Question + "\n" + strings.Join(it.Sources, "\n") + "\n" + it.Body)); rule != "" {
			skips = append(skips, Skip{it.ID, "deposit matches the " + rule + " secret pattern; not researched, a human must look at it"})
			continue
		}
		rawRel := path.Join(IntakeDir, it.ID+".md")
		if err := writeWithin(root, rawRel, []byte(it.Body)); err != nil {
			return nil, nil, err
		}
		pageID := path.Join(CandidateDir, it.ID)
		pageRel := pagePathForRepo(opts.WikiDir, pageID)
		// Caller-supplied fields reach the prompt only as labelled data
		// blocks, and only the sources an agent may open (meerkat-mob#34).
		sources, dropped := researchableSources(it.Sources)
		srcLabel := "Sources it cited (https, public hosts only)"
		if dropped > 0 {
			srcLabel += fmt.Sprintf("; %d more were not https to a public host and are left out, do not look for them", dropped)
		}
		subs := map[string]string{
			"raw_path":  rawRel,
			"intake_id": it.ID,
			"question":  untrustedBlock("The question the agent asked", []string{it.Question}),
			"attempted": untrustedBlock("Collections it tried, in order", it.Attempted),
			"sources":   untrustedBlock(srcLabel, sources),
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
	root, err := os.OpenRoot(opts.Workdir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
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
		// The staged copy is the candidate; the working copy is only where
		// the validator edits it. Independence is judged on the staged
		// page and the recorded runs, never on what an earlier run left in
		// the working copy (meerkat-mob#33).
		page, err := kb.ParsePage(pageID, pageRel, s.Body)
		if err != nil {
			skips = append(skips, Skip{s.ID, "candidate does not parse: " + err.Error()})
			continue
		}
		rm := researcherModel(page)
		if rm != "" && rm == opts.Model {
			skips = append(skips, Skip{s.ID, "validator must not run the researcher's model (" + rm + "); pass a different --model"})
			continue
		}
		vals, err := store.Validations(ctx, s.ID)
		if err != nil {
			return nil, nil, err
		}
		models := intake.ConfirmingModels(vals, rm)
		if len(models) >= ConfirmationsRequired {
			skips = append(skips, Skip{s.ID, "already has the required confirmations"})
			continue
		}
		if slices.Contains(models, opts.Model) {
			skips = append(skips, Skip{s.ID, "model " + opts.Model + " already confirmed this candidate; pass a different --model"})
			continue
		}
		// Every run starts from the staged copy, so whatever the page says
		// after the run is this run's work and nothing an earlier run left
		// behind is carried in. Lstat through the root: a name that is, or
		// resolves through, a link (left by an earlier run)
		// is skipped, never followed or replaced (#92).
		switch fi, err := root.Lstat(filepath.FromSlash(pageRel)); {
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			skips = append(skips, Skip{s.ID, "candidate path escapes the working copy: " + err.Error()})
			continue
		case err == nil && !fi.Mode().IsRegular():
			skips = append(skips, Skip{s.ID, "candidate path escapes the working copy: not a regular file (" + fi.Mode().Type().String() + ")"})
			continue
		}
		if err := writeWithin(root, pageRel, s.Body); err != nil {
			skips = append(skips, Skip{s.ID, err.Error()})
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

// researcherModel is the model the pipeline stamped on a staged
// candidate.
func researcherModel(p kb.Page) string {
	rm, _ := p.Front.Extra["researcher_model"].(string)
	return rm
}

func substitute(tpl string, subs map[string]string) string {
	// Every {{key}} is replaced in ONE pass (strings.Replacer), so a
	// value that itself contains "{{other}}" is never expanded: what a
	// value says is data, whatever order the keys come in
	// (meerkat-mob#35).
	keys := make([]string, 0, len(subs))
	for k := range subs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, "{{"+k+"}}", subs[k])
	}
	return strings.NewReplacer(pairs...).Replace(tpl)
}

// writeWithin writes rel under root, creating directories on the way.
//
// Everything goes through the os.Root (#92): a symlink left in a
// persistent working copy by an earlier run — at rel itself or at any
// directory on the way — cannot redirect the write out of the tree,
// because the root re-resolves every component against the open
// directory and refuses to leave it. And whatever already sits at rel is
// unlinked first (the name, not its target), so a link there, even one
// that stays inside the tree, is replaced rather than written through.
// The same reasoning as Finalize and FinalizeRewrites (#74, #91).
func writeWithin(root *os.Root, rel string, body []byte) error {
	name := filepath.FromSlash(rel)
	if err := root.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return fmt.Errorf("path %q escapes the working copy: %w", rel, err)
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("path %q escapes the working copy: %w", rel, err)
	}
	return root.WriteFile(name, body, 0o600)
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
	// SECURITY: every candidate read and write below goes through an os.Root
	// opened on the working copy, addressed by the page's RELATIVE path —
	// never by a string-joined absolute one. isPathWithinBase (kept below as
	// the cheap pre-check) compares filepath.Abs/Rel results and therefore
	// cannot see symlinks: a link planted inside the working copy by the very
	// agent run that produced these results would pass it and redirect the
	// write outside the tree. os.Root re-resolves every path component
	// against the open directory and refuses to leave it, so containment is
	// enforced by the OS rather than by string comparison — the same
	// reasoning as internal/kbdir, internal/memory and internal/contentsource.
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

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
		rel := filepath.FromSlash(t.PagePath)
		if !isPathWithinBase(workdir, filepath.Join(workdir, rel)) {
			f.Action, f.Detail = "failed", "candidate path escapes the working copy"
			out = append(out, f)
			continue
		}
		body, err := root.ReadFile(rel)
		if err != nil {
			// os exports no sentinel for os.Root's containment refusal
			// (os.ErrPathEscapes exists only inside the os package's own
			// tests), so anything that is not a plain "not found" is
			// reported as a containment failure and carries the underlying
			// error so the operator still sees what the kernel said.
			if errors.Is(err, fs.ErrNotExist) {
				f.Action, f.Detail = "failed", "candidate page missing after run: "+err.Error()
			} else {
				f.Action, f.Detail = "failed", "candidate path escapes the working copy: "+err.Error()
			}
			out = append(out, f)
			continue
		}
		if t.Role == RoleResearcher {
			if rule := findSecret(body); rule != "" {
				// Not staged, and taken out of the working copy so a later
				// commit does not carry it. What the agent already pushed is
				// the operator's to revert.
				_ = root.Remove(rel)
				f.Action, f.Detail = "failed", "candidate matches the "+rule+" secret pattern; not staged and removed from the working copy (revert the agent's commit if it pushed one)"
				out = append(out, f)
				continue
			}
		}
		page, err := kb.ParsePage(t.PageID, t.PagePath, body)
		if err != nil {
			f.Action, f.Detail = "failed", "candidate does not parse: "+err.Error()
			out = append(out, f)
			continue
		}
		switch t.Role {
		case RoleResearcher:
			stamped := stampCandidate(page, t, now)
			if err := root.WriteFile(rel, stamped, 0o600); err != nil {
				return out, err
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
			if err := finalizeValidation(ctx, store, root, rel, t, page, now, &f); err != nil {
				return out, err
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// finalizeValidation judges one validator run against the staged
// candidate it started from and records it. The run may change only the
// fields a validator owns (see validatorChangedOnly); anything else
// restores the staged copy and fails the run without recording it. What
// counts is the pipeline's record of the run (its model and outcome) in
// the intake store; the verified: entry the run wrote is only the signal
// that it confirmed (meerkat-mob#33).
func finalizeValidation(ctx context.Context, store *intake.Store, root *os.Root, rel string, t Task, after kb.Page, now time.Time, f *Finalization) error {
	staged, ok, err := stagedFor(ctx, store, t.TargetKB, t.IntakeID)
	if err != nil {
		return err
	}
	if !ok {
		f.Action, f.Detail = "failed", "no staged candidate for this validator run"
		return nil
	}
	before, err := kb.ParsePage(t.PageID, t.PagePath, staged)
	if err != nil {
		f.Action, f.Detail = "failed", "staged candidate does not parse: "+err.Error()
		return nil
	}
	if reason := validatorChangedOnly(before, after); reason != "" {
		if err := restorePage(root, rel, staged); err != nil {
			return err
		}
		f.Action, f.Detail = "failed", "validator run rejected: "+reason+"; staged copy restored"
		return nil
	}
	rm := researcherModel(before)
	if t.Model == "" || t.Model == rm {
		f.Action, f.Detail = "failed", "validator run has no model distinct from the researcher's; not recorded"
		return nil
	}
	run := intake.Validation{Run: intake.NewRunID(now), Model: t.Model, At: now.UTC()}
	confirmed := after.Front.FailureReason == "" && len(after.Front.Verified) > len(before.Front.Verified)
	if confirmed {
		run.Outcome = intake.ValidationConfirmed
	} else {
		run.Outcome, run.Reason = intake.ValidationFailed, after.Front.FailureReason
		if run.Reason == "" {
			run.Reason = "the validator neither verified nor failed the candidate"
		}
	}
	if err := store.RecordValidation(ctx, t.IntakeID, run); err != nil {
		return err
	}
	vals, err := store.Validations(ctx, t.IntakeID)
	if err != nil {
		return err
	}
	switch {
	case !confirmed && strings.HasPrefix(strings.ToLower(run.Reason), NeedsHumanPrefix):
		if err := store.Park(ctx, t.IntakeID, run.Reason); err != nil {
			return err
		}
		telemetry.Record(ctx).LibrarianFinding("needs_human")
		f.Action, f.Detail = "parked", run.Reason
	case !confirmed:
		n := len(intake.Failures(vals))
		if n >= ConfirmationsRequired {
			reason := fmt.Sprintf("validators disagreed %d times; last: %s", n, run.Reason)
			if err := store.Park(ctx, t.IntakeID, reason); err != nil {
				return err
			}
			telemetry.Record(ctx).LibrarianFinding("needs_human")
			f.Action, f.Detail = "parked", reason
		} else {
			f.Action, f.Detail = "pending", fmt.Sprintf("validation failed (%d of %d): %s", n, ConfirmationsRequired, run.Reason)
		}
	default:
		models := intake.ConfirmingModels(vals, rm)
		if len(models) < ConfirmationsRequired {
			f.Action, f.Detail = "pending", fmt.Sprintf("%d of %d confirmations", len(models), ConfirmationsRequired)
			break
		}
		if err := store.MarkDone(ctx, "validated-"+t.IntakeID, "confirmed by "+strings.Join(verifierNames(models), ", ")); err != nil {
			return err
		}
		f.Action, f.Detail = "confirmed", fmt.Sprintf("%d independent confirmations; ready to file into %q", len(models), t.TargetKB)
	}
	return nil
}

// validatorChangedOnly returns "" when a validator run changed nothing
// but the fields a validator owns (verified, failure_reason, status),
// else what else it changed.
func validatorChangedOnly(before, after kb.Page) string {
	if strings.TrimSpace(before.Body) != strings.TrimSpace(after.Body) {
		return "the body changed"
	}
	fb, fa := before.Front, after.Front
	fb.Verified, fa.Verified = nil, nil
	fb.FailureReason, fa.FailureReason = "", ""
	fb.Status, fa.Status = "", ""
	// Compare through the renderer, as onlyFieldChanged does, so a
	// re-render's formatting is not read as a change.
	if !bytes.Equal(renderPage(kb.Page{Front: fb}), renderPage(kb.Page{Front: fa})) {
		return "a frontmatter field other than verified, failure_reason or status changed"
	}
	return ""
}

// stagedFor reads the staged candidate for kb/id.
func stagedFor(ctx context.Context, store *intake.Store, kbName, id string) ([]byte, bool, error) {
	staged, err := store.ListStaged(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, s := range staged {
		if s.KB == kbName && s.ID == id {
			return s.Body, true, nil
		}
	}
	return nil, false, nil
}

// verifierNames is how confirming models are named in verified: entries.
func verifierNames(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, AgentVerifierPrefix+string(RoleValidator)+":"+m)
	}
	return out
}

// Confirmations returns the distinct models whose pipeline-recorded
// validator runs confirmed a staged candidate. The candidate's own
// verified: entries are never consulted: they are agent-written text.
func Confirmations(ctx context.Context, store *intake.Store, s intake.Staged) ([]string, error) {
	vals, err := store.Validations(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	rm := ""
	if p, err := kb.ParsePage(s.ID, s.Key, s.Body); err == nil {
		rm = researcherModel(p)
	}
	return intake.ConfirmingModels(vals, rm), nil
}

// withRecordedVerification is the page a confirmed candidate is filed
// as: the staged copy with verified: rebuilt from the recorded runs.
func withRecordedVerification(ctx context.Context, store *intake.Store, s intake.Staged) ([]byte, error) {
	p, err := kb.ParsePage(s.ID, s.Key, s.Body)
	if err != nil {
		return nil, err
	}
	vals, err := store.Validations(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	models := intake.ConfirmingModels(vals, researcherModel(p))
	p.Front.Verified = nil
	for _, m := range models {
		for _, v := range vals {
			if v.Model == m && v.Outcome == intake.ValidationConfirmed {
				p.Front.Verified = append(p.Front.Verified, kb.Verifier{By: verifierNames([]string{m})[0], At: v.At.UTC().Format(time.RFC3339)})
				break
			}
		}
	}
	if len(models) >= ConfirmationsRequired {
		p.Front.Status = "machine-confirmed"
	}
	return renderPage(p), nil
}

// stampCandidate makes a researcher's page into the staged candidate.
// The researcher is an agent working from caller-supplied research, so
// everything in its frontmatter the pipeline relies on is reset rather
// than trusted (meerkat-mob#33): verified: is emptied (only recorded
// validator runs confirm), status is unverified, failure_reason is
// cleared, generated and last_ingested are the pipeline's, and every key
// outside the OKF core is dropped before intake_id, researcher_model and
// target_kb are stamped.
func stampCandidate(p kb.Page, t Task, now time.Time) []byte {
	at := now.UTC().Format(time.RFC3339)
	p.Front.Verified = nil
	p.Front.Status = "unverified"
	p.Front.FailureReason = ""
	p.Front.Generated = &kb.Generated{By: AgentVerifierPrefix + string(RoleResearcher), At: at}
	p.Front.LastIngested = at
	p.Front.Extra = map[string]any{
		"intake_id":        t.IntakeID,
		"researcher_model": t.Model,
		"target_kb":        t.TargetKB,
	}
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
