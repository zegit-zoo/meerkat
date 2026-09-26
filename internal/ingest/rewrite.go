package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// rewrite.go is the librarian's prompt-quality rewrite stage
// (meerkat-mob issue #21, second half). The analysis in
// promptquality.go says which pointer hints and page descriptions do
// not carry the words agents use; this stage hands each one to the
// executor as a bounded task — one file, one frontmatter field — and
// accepts the result only if that is all that changed.
//
// Tool-description findings are not executed: the tool text lives in
// meerkat's own source, not in a content repo, so they become a
// proposal file in the working copy for a human to turn into a merge
// request. Nothing here touches a running server.

// Fields a rewrite may change.
const (
	FieldHint        = "hint"
	FieldDescription = "description"
	// maxRewriteFieldLen bounds the rewritten value; pointer hints are
	// capped at 300 by kb already.
	maxRewriteFieldLen = 300
	// ToolProposalDir (under the working copy) receives tool-description
	// proposals.
	ToolProposalDir = "ingestion/proposals"
	// DefaultRewriteBranch is the branch rewrites are committed to when
	// none is given: a review branch, never the content branch itself,
	// so a rewrite is confirmed by review before it serves.
	DefaultRewriteBranch = "librarian/rewrites"
	rewritePromptName    = "librarian-rewrite"
)

// RewritePlanOpts plans rewrite tasks from a report.
type RewritePlanOpts struct {
	// Workdir is the content working copy; findings whose page is not
	// in it are skipped, not failed.
	Workdir      string
	WikiDir      string
	Model        string
	SubagentType string
	WallClockCap int
	Now          func() time.Time
}

// rewritePrompt is RolePrompt for the rewrite brief: the content repo's
// ingestion/prompts/librarian-rewrite.md when present, else the
// built-in.
func rewritePrompt() (string, error) {
	if p, err := sources.Prompt(rewritePromptName + ".md"); err == nil && strings.TrimSpace(p) != "" {
		return p, nil
	}
	b, err := defaultPrompts.ReadFile("prompts/" + rewritePromptName + ".md")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// PlanRewrites turns the report's hint, description and route findings
// into one task per page. A finding whose page is not in the working
// copy (another repo in the tree, or a memory-overlay page) is skipped
// with the reason.
func PlanRewrites(rep *Report, opts RewritePlanOpts) ([]Task, []Skip, error) {
	if opts.Workdir == "" {
		return nil, nil, errors.New("a content working copy is required (--workdir-kb)")
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
	prompt, err := rewritePrompt()
	if err != nil {
		return nil, nil, err
	}
	var tasks []Task
	var skips []Skip
	seen := map[string]bool{}
	for _, f := range rep.PromptQuality {
		field, reason := "", ""
		switch f.Target {
		case TargetHint:
			field, reason = FieldHint, "sessions gave up in the target collection; the hint does not say what is there in the words agents use"
		case TargetRoute:
			field, reason = FieldHint, "sessions starting in this hub took a wrong turn; the hints do not separate its children"
		case TargetDescription:
			field, reason = FieldDescription, "sessions found this page only after the exact stage missed; its description lacks the words agents use"
		default:
			continue
		}
		for _, qualified := range f.Pages {
			coll, id, ok := strings.Cut(qualified, ":")
			if !ok || seen[qualified] {
				continue
			}
			seen[qualified] = true
			rel := pagePathForRepo(opts.WikiDir, id)
			full := filepath.Join(opts.Workdir, filepath.FromSlash(rel))
			if !isPathWithinBase(opts.Workdir, full) {
				skips = append(skips, Skip{qualified, "page path escapes the working copy"})
				continue
			}
			if _, err := os.Stat(full); err != nil {
				skips = append(skips, Skip{qualified, "not in this working copy (" + rel + ")"})
				continue
			}
			queries := make([]string, 0, len(f.Queries))
			for _, q := range f.Queries {
				queries = append(queries, "- "+q)
			}
			subs := map[string]string{
				"page_path":  rel,
				"page_id":    id,
				"collection": coll,
				"field":      field,
				"reason":     reason,
				"queries":    strings.Join(queries, "\n"),
				"terms":      strings.Join(f.Terms, ", "),
			}
			tasks = append(tasks, Task{
				Role: RoleLibrarian, IntakeID: "rewrite:" + qualified, TargetKB: coll,
				PageID: id, PagePath: rel, SourceID: "librarian", Field: field, Queries: f.Queries,
				Prompt: substitute(prompt, subs), Model: opts.Model, SubagentType: opts.SubagentType, WallClockCapS: opts.WallClockCap,
			})
		}
	}
	return tasks, skips, nil
}

// buildRewriteInstruction wraps the brief with the one-file contract.
func buildRewriteInstruction(t Task, workdir, branch string) string {
	return fmt.Sprintf(`You are the Meerkat librarian agent on **one rewrite**.
Working directory: %s
File: `+"`%s`"+`  Field: `+"`%s:`"+`
Change that one field only, commit + push, then stop.
**IMPORTANT: Do this work yourself. DO NOT use the Task tool to delegate.**
After editing, run (in this order, with retry-on-conflict up to 3 times):
    git pull --rebase --quiet || true
    git add %s
    git commit -m "%s"
    git push origin %s
When done, print exactly: REWRITE_DONE: %s
Then stop.
---
%s`, workdir, t.PagePath, t.Field, t.PagePath, rewriteCommitMessage(t), branch, t.PageID, t.Prompt)
}

// rewriteCommitMessage cites the queries that motivated the edit.
func rewriteCommitMessage(t Task) string {
	q := t.Queries
	if len(q) > 3 {
		q = q[:3]
	}
	quoted := make([]string, len(q))
	for i, s := range q {
		quoted[i] = fmt.Sprintf("%q", strings.ReplaceAll(s, `"`, "'"))
	}
	return fmt.Sprintf("librarian(rewrite): %s %s for queries %s", t.Field, idBasename(t.PageID), strings.Join(quoted, ", "))
}

// Snapshot reads every task's page before the run so FinalizeRewrites
// can tell exactly what changed. Keyed by PagePath.
//
// Reads go through an os.Root over the working copy, like every read and
// write in FinalizeRewrites (issue #91): os.Root re-resolves each path
// component against the open directory and refuses to leave it, so
// containment is enforced by the OS rather than by string comparison.
func Snapshot(workdir string, tasks []Task) (map[string][]byte, error) {
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	out := map[string][]byte{}
	for _, t := range tasks {
		if t.Role != RoleLibrarian {
			continue
		}
		rel := filepath.FromSlash(t.PagePath)
		if !isPathWithinBase(workdir, filepath.Join(workdir, rel)) {
			return nil, fmt.Errorf("%s escapes the working copy", t.PagePath)
		}
		b, err := readPage(root, rel)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.PagePath, err)
		}
		out[t.PagePath] = b
	}
	return out, nil
}

// errNotAPage is readPage's refusal of anything but a single regular
// file.
var errNotAPage = errors.New("not a single regular file")

// readPage reads the page at rel under root, and only if it is a single
// regular file. A rewrite edits one field of the page it was given, so a
// run that left a symlink there — even one that stays inside the tree —
// or a hard link with a second name has replaced the page rather than
// edited it. Reading through either would compare, and report, another
// file's content; restoring through either would overwrite that file.
func readPage(root *os.Root, rel string) ([]byte, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: mode %s", errNotAPage, fi.Mode().Type())
	}
	if n := linkCount(fi); n > 1 {
		return nil, fmt.Errorf("%w: %d hard links", errNotAPage, n)
	}
	return root.ReadFile(rel)
}

// restorePage puts the snapshot back at rel as a fresh regular file.
// Whatever the run left there is unlinked first — the name, not what it
// points at — so a planted symlink or hard link is never written
// through.
func restorePage(root *os.Root, rel string, orig []byte) error {
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.WriteFile(rel, orig, 0o600)
}

// Rewritten is what FinalizeRewrites decided about one task.
type Rewritten struct {
	Page   string // qualified
	Field  string
	Action string // rewritten | unchanged | rejected | failed
	Detail string
	Before string
	After  string
}

// FinalizeRewrites checks each executed rewrite against its snapshot:
// the page must parse, the body and every other frontmatter field
// must be byte-for-byte what they were, and the field must have
// changed and stay within the length cap. A violation restores the
// snapshot (the agent's commit, if any, is on the review branch and
// is the operator's to discard) and is reported as rejected.
//
// Every read and write goes through an os.Root over the working copy
// (issue #91), and the page must still be a single regular file: see
// readPage and restorePage.
func FinalizeRewrites(ctx context.Context, workdir string, results []Result, before map[string][]byte) ([]Rewritten, error) {
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	var out []Rewritten
	m := telemetry.Record(ctx)
	for _, r := range results {
		t := r.Task
		if t.Role != RoleLibrarian || t.Field == "" {
			continue
		}
		rw := Rewritten{Page: t.TargetKB + ":" + t.PageID, Field: t.Field}
		if r.ExitStatus != "ok" {
			rw.Action, rw.Detail = "failed", r.FailReason
			m.LibrarianRewrite(rw.Action)
			out = append(out, rw)
			continue
		}
		rel := filepath.FromSlash(t.PagePath)
		if !isPathWithinBase(workdir, filepath.Join(workdir, rel)) {
			rw.Action, rw.Detail = "failed", "page path escapes the working copy"
			m.LibrarianRewrite(rw.Action)
			out = append(out, rw)
			continue
		}
		orig, ok := before[t.PagePath]
		if !ok {
			rw.Action, rw.Detail = "failed", "no snapshot for the page"
			m.LibrarianRewrite(rw.Action)
			out = append(out, rw)
			continue
		}
		after, err := readPage(root, rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			rw.Action, rw.Detail = "failed", "page missing after run: "+err.Error()
			m.LibrarianRewrite(rw.Action)
			out = append(out, rw)
			continue
		case err != nil:
			// The run replaced the page with a link, or a path component
			// now leads out of the working copy. os exports no sentinel
			// for os.Root's containment refusal, so the underlying error
			// is carried for the operator either way.
			if rerr := restorePage(root, rel, orig); rerr != nil {
				rw.Action, rw.Detail = "failed", "page path escapes the working copy: "+err.Error()+"; snapshot NOT restored: "+rerr.Error()
			} else {
				rw.Action, rw.Detail = "rejected", "the page was replaced ("+err.Error()+"); snapshot restored"
			}
			m.LibrarianRewrite(rw.Action)
			out = append(out, rw)
			continue
		}
		if bytes.Equal(orig, after) {
			rw.Action, rw.Detail = "unchanged", "the executor changed nothing"
			m.LibrarianRewrite(rw.Action)
			out = append(out, rw)
			continue
		}
		reason, beforeV, afterV := onlyFieldChanged(t, orig, after)
		rw.Before, rw.After = beforeV, afterV
		if reason != "" {
			if err := restorePage(root, rel, orig); err != nil {
				return out, err
			}
			rw.Action, rw.Detail = "rejected", reason+"; snapshot restored"
		} else {
			rw.Action, rw.Detail = "rewritten", fmt.Sprintf("%s: %q -> %q", t.Field, beforeV, afterV)
		}
		m.LibrarianRewrite(rw.Action)
		out = append(out, rw)
	}
	return out, nil
}

// onlyFieldChanged parses both versions and returns "" when the only
// difference is the task's field, else why not.
func onlyFieldChanged(t Task, orig, after []byte) (reason, beforeV, afterV string) {
	pb, err := kb.ParsePage(t.PageID, t.PagePath, orig)
	if err != nil {
		return "snapshot does not parse: " + err.Error(), "", ""
	}
	pa, err := kb.ParsePage(t.PageID, t.PagePath, after)
	if err != nil {
		return "page does not parse after the run: " + err.Error(), "", ""
	}
	get := func(p kb.Page) string {
		if t.Field == FieldHint {
			return p.Front.Hint
		}
		return p.Front.Description
	}
	beforeV, afterV = get(pb), get(pa)
	if pb.Body != pa.Body {
		return "the body changed", beforeV, afterV
	}
	fb, fa := pb.Front, pa.Front
	if t.Field == FieldHint {
		fb.Hint, fa.Hint = "", ""
	} else {
		fb.Description, fa.Description = "", ""
	}
	// Compare through the renderer, so nil and empty lists (a hand
	// edit keeps `tags:` absent, a re-render writes `tags: []`) read as
	// the same frontmatter.
	if !bytes.Equal(renderPage(kb.Page{Front: fb}), renderPage(kb.Page{Front: fa})) {
		return "a frontmatter field other than " + t.Field + " changed", beforeV, afterV
	}
	if strings.TrimSpace(afterV) == "" {
		return t.Field + " is empty", beforeV, afterV
	}
	if strings.Contains(afterV, "\n") || len(afterV) > maxRewriteFieldLen {
		return fmt.Sprintf("%s must be one line under %d characters", t.Field, maxRewriteFieldLen), beforeV, afterV
	}
	if afterV == beforeV {
		return "", beforeV, afterV // formatting-only change; reported as rewritten with equal values
	}
	return "", beforeV, afterV
}

// WriteToolProposal writes the tool-target findings as a proposal file
// in the working copy for a human to turn into a merge request on
// meerkat. Returns the path written, or "" when there was nothing.
func WriteToolProposal(rep *Report, workdir string, now time.Time) (string, error) {
	var findings []PromptFinding
	for _, f := range rep.PromptQuality {
		if f.Target == TargetTool {
			findings = append(findings, f)
		}
	}
	if len(findings) == 0 {
		return "", nil
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Sessions > findings[j].Sessions })
	var b strings.Builder
	fmt.Fprintf(&b, "# Tool-description proposal — %s\n\n", now.UTC().Format("2006-01-02"))
	b.WriteString("Sessions gave up without leaving tier 0: the `mk_search` tool description did not send them anywhere.\n")
	b.WriteString("Proposed change to the tool text in `internal/mcp/server.go` (meerkat), to be filed as a merge request; never applied to a running server.\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "## %d sessions\n\n", f.Sessions)
		for _, q := range f.Queries {
			fmt.Fprintf(&b, "- %q\n", q)
		}
		b.WriteString("\nSuggested addition: name the collection (or pointer) that answers these, in the words above, so the first search is scoped rather than unqualified.\n\n")
	}
	rel := path.Join(ToolProposalDir, "tool-description-"+now.UTC().Format("2006-01-02")+".md")
	if err := writeWithin(workdir, rel, []byte(b.String())); err != nil {
		return "", err
	}
	return rel, nil
}
