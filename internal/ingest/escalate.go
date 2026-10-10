package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/forge"
	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// escalate.go is the librarian's forge escalation (meerkat-mob #19,
// MK-LIB-09): a parked (needs-human) intake item becomes ONE issue on
// the target collection's forge, with the disagreement summarised, and
// the parked marker is removed when a human closes that issue as
// resolved.
// Nothing waits for the human (MK-INT-06): filing and checking happen on
// the next `mk ingest --role librarian --apply`, and every reason not to
// file — no forge, no token — is reported, never a failed run.

// ResolvedLabel is the label a human puts on a closed issue to un-park
// its item. Closing without it leaves the item parked: "won't fix" and
// "duplicate" are not answers a validator can re-run against.
const ResolvedLabel = "resolved"

// NeedsHumanLabel is put on every filed issue.
const NeedsHumanLabel = "needs-human"

// maxIssueTitle bounds an issue title; the candidate title is agent text.
const maxIssueTitle = 120

// filingStale is how old another run's filing claim must be before this
// run takes it over: longer than a run spends between claiming and
// recording (a few bounded forge calls), so a fresh claim means a run
// is filing now and an old one means a run died mid-filing.
const filingStale = 15 * time.Minute

// findSlack widens the look-back for an issue an interrupted run may have
// filed, for clock skew between this host and the forge.
const findSlack = 10 * time.Minute

// ParkedEntry is one parked item and what an issue about it says.
type ParkedEntry struct {
	IntakeID string `json:"intake_id"`
	// Collection is the target collection: the staged candidate's, else
	// the raw item's recorded target, else "unrouted".
	Collection string `json:"collection"`
	Reason     string `json:"reason"`
	// Title is the candidate page's title, when there is a candidate.
	Title          string   `json:"title,omitempty"`
	Question       string   `json:"question,omitempty"`
	Attempted      []string `json:"attempted,omitempty"`
	FailureReasons []string `json:"failure_reasons,omitempty"`
	// Issue is the filed forge issue, nil until one is filed.
	Issue *intake.IssueRef `json:"issue,omitempty"`
	// Filing is when a run began filing an issue it has not recorded.
	Filing time.Time `json:"filing,omitzero"`
	// IssueErr says why the marker's issue section is unreadable.
	IssueErr string `json:"issue_error,omitempty"`
}

func (p ParkedEntry) detail() string {
	switch {
	case p.IssueErr != "":
		return "issue section unreadable (" + p.IssueErr + "); " + p.Reason
	case p.Issue != nil:
		return "issue " + p.Issue.URL + " open; " + p.Reason
	case !p.Filing.IsZero():
		return "issue filing began " + p.Filing.UTC().Format(time.RFC3339) + ", not recorded; " + p.Reason
	}
	return "issue not yet filed; " + p.Reason
}

// parkedEntries reads the parked markers and fills in what an issue
// needs from the staged candidate and the raw deposit.
func parkedEntries(ctx context.Context, store *intake.Store, staged []intake.Staged) ([]ParkedEntry, error) {
	items, err := store.ParkedDetail(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ParkedEntry, 0, len(items))
	for _, it := range items {
		p := ParkedEntry{IntakeID: it.ID, Reason: it.Reason, Issue: it.Issue, Filing: it.Filing, IssueErr: it.IssueErr}
		for _, s := range staged {
			if s.ID != it.ID {
				continue
			}
			p.Collection = s.KB
			if page, err := kb.ParsePage(s.ID, s.Key, s.Body); err == nil {
				p.Title = page.Title
				if page.Front.FailureReason != "" {
					p.FailureReasons = append(p.FailureReasons, page.Front.FailureReason)
				}
			}
			break
		}
		// The validators' reasons, as the pipeline recorded their runs.
		if vals, err := store.Validations(ctx, it.ID); err == nil {
			for _, v := range intake.Failures(vals) {
				if v.Reason != "" && !slices.Contains(p.FailureReasons, v.Reason) {
					p.FailureReasons = append(p.FailureReasons, v.Reason)
				}
			}
		}
		raw, ok, err := store.FindRaw(ctx, it.ID)
		if err != nil {
			// A malformed deposit still gets its issue, without the
			// question; the reason is the part a human needs.
			ok = false
		}
		if ok {
			p.Question, p.Attempted = raw.Question, raw.Attempted
			if p.Collection == "" {
				p.Collection = targetKB(raw)
			}
		}
		if p.Collection == "" {
			p.Collection = unrouted
		}
		out = append(out, p)
	}
	return out, nil
}

// ApplyOpts are Apply's seams.
type ApplyOpts struct {
	// Forge builds the client for a resolved target; nil is forge.New.
	Forge func(t forge.Target, token string) (forge.Client, error)
	// Getenv reads the contract's token_env; nil is os.Getenv.
	Getenv func(string) string
	// Now is the clock filing claims are stamped with; nil is time.Now.
	Now func() time.Time
	// FileConfirmed lets Apply write confirmed candidates into a
	// `direct` collection's memory store. Off by default: two agent
	// confirmations are a machine check, and publishing the page to
	// every reader of the collection is a human's call (meerkat-mob#33).
	FileConfirmed bool
}

// applyParked files an issue for every parked item that has none, and
// un-parks every item whose issue is closed with the resolved label.
func applyParked(ctx context.Context, reg *collections.Registry, store *intake.Store, rep *Report, opts ApplyOpts, out []Applied) ([]Applied, error) {
	if opts.Forge == nil {
		opts.Forge = forge.New
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	for _, p := range rep.Parked {
		a := Applied{IntakeID: p.IntakeID}
		marker := intake.ParkedKey(p.IntakeID)
		if p.IssueErr != "" {
			a.Action, a.Detail = "skipped", "the "+marker+" issue section is unreadable ("+p.IssueErr+"); repair or remove it by hand"
			out = append(out, a)
			continue
		}
		spec := forgeSpec(reg, p.Collection)
		if spec == nil {
			if p.Issue == nil {
				a.Action, a.Detail = "instructions", "no forge for "+p.Collection+"; a human must look at "+marker
				out = append(out, a)
			}
			continue
		}
		if p.Issue == nil {
			// The issue goes to this collection's forge with its token:
			// only for the target the deposit was authorised for.
			if why := depositTargetMismatch(ctx, store, p.IntakeID, p.Collection); why != "" {
				a.Action, a.Detail = "skipped", "no issue filed on the forge of "+p.Collection+": "+why+"; a human must look at "+marker
				out = append(out, a)
				continue
			}
		}
		target, err := forge.Resolve(spec.Host, spec.Repo)
		if err != nil {
			a.Action, a.Detail = "skipped", fmt.Sprintf("no issue for %s: %v; a human must look at %s", p.Collection, err, marker)
			out = append(out, a)
			continue
		}
		if spec.TokenEnv == "" {
			a.Action, a.Detail = "skipped", "the update contract of "+p.Collection+" names no token_env; no issue filed, a human must look at "+marker
			out = append(out, a)
			continue
		}
		token := opts.Getenv(spec.TokenEnv)
		if token == "" {
			a.Action, a.Detail = "skipped", "$"+spec.TokenEnv+" is not set; no forge call made for "+marker
			out = append(out, a)
			continue
		}
		client, err := opts.Forge(target, token)
		if err != nil {
			a.Action, a.Detail = "skipped", "forge client: "+err.Error()
			out = append(out, a)
			continue
		}
		if p.Issue != nil {
			if done := checkIssue(ctx, store, client, target, p); done.Action != "" {
				out = append(out, done)
			}
			continue
		}
		a, err = fileIssue(ctx, store, client, target, p, opts.Now())
		if err != nil {
			return out, err
		}
		out = append(out, a)
	}
	return out, nil
}

// fileIssue files the issue for one parked item that has none. It first
// claims the filing in the marker (intake.Store.BeginFiling), so a
// concurrent run backs off; when an earlier claim was never completed,
// it looks for the issue that attempt may have created (a timeout after
// the forge created it) and adopts it instead of filing again. Every
// outcome but "filed and not recorded" is an Applied, never an error.
func fileIssue(ctx context.Context, store *intake.Store, client forge.Client, target forge.Target, p ParkedEntry, now time.Time) (Applied, error) {
	a := Applied{IntakeID: p.IntakeID}
	marker := intake.ParkedKey(p.IntakeID)
	prior, err := store.BeginFiling(ctx, p.IntakeID, now, filingStale)
	var already *intake.AlreadyFiledError
	switch {
	case errors.As(err, &already):
		a.Action, a.Detail = "skipped", "issue "+already.Ref.URL+" was recorded by another run; nothing filed"
		return a, nil
	case errors.Is(err, intake.ErrFilingInFlight):
		a.Action, a.Detail = "skipped", "nothing filed: "+err.Error()+" (another run filing now, or an attempt that failed); a claim older than "+filingStale.String()+" is retried"
		return a, nil
	case err != nil:
		a.Action, a.Detail = "skipped", "claim the filing: "+err.Error()
		return a, nil
	}
	ref := intake.IssueRef{Host: target.Host, API: target.APIBase, Repo: target.Repo}
	note := ""
	if !prior.IsZero() {
		url, number, found, err := client.FindIssue(ctx, target.Repo, issueMarker(p.IntakeID), prior.Add(-findSlack))
		switch {
		case found:
			ref.URL, ref.Number = url, number
		case errors.Is(err, forge.ErrSearchTruncated):
			note = "; an earlier, interrupted filing could not be ruled out (" + err.Error() + "), check for a duplicate"
		case err != nil:
			a.Action, a.Detail = "skipped", "look for the issue an interrupted run may have filed: "+err.Error()
			return a, nil
		}
	}
	action := "adopted"
	if ref.Number == 0 {
		url, number, err := client.CreateIssue(ctx, target.Repo, issueTitle(p), issueBody(p, marker), []string{NeedsHumanLabel})
		if err != nil {
			// The claim stays: the forge may have created the issue before
			// failing, and the next run looks for it before filing again.
			a.Action, a.Detail = "skipped", "file issue: "+err.Error()
			return a, nil
		}
		telemetry.Record(ctx).LibrarianFiledIssue(target.Host)
		ref.URL, ref.Number, action = url, number, "filed"
	}
	if err := store.SetParkedIssue(ctx, p.IntakeID, ref); err != nil {
		if errors.As(err, &already) {
			a.Action, a.Detail = "duplicate", "issue "+ref.URL+" duplicates "+already.Ref.URL+", which another run recorded first; close "+ref.URL+" by hand"
			return a, nil
		}
		return a, fmt.Errorf("filed %s for %s but could not record it in %s (the next run looks for it before filing again; if that fails too, add it by hand): %w", ref.URL, p.IntakeID, marker, err)
	}
	a.Action, a.Detail = action, "issue "+ref.URL+note
	return a, nil
}

// checkIssue reads a filed issue and un-parks its item when it is closed
// with the resolved label ("unparked"). A closed-but-unresolved issue or
// a forge error is "skipped" with the reason; an open issue returns the
// zero Applied, because there is nothing to say yet.
func checkIssue(ctx context.Context, store *intake.Store, client forge.Client, target forge.Target, p ParkedEntry) Applied {
	a := Applied{IntakeID: p.IntakeID}
	// SECURITY: only an issue on the forge the contract names today is
	// trusted to un-park. A stale or hand-edited reference pointing
	// elsewhere is reported, not followed. The host kind alone does not
	// name a forge (github.com and a GitHub Enterprise server are both
	// "github"), so the API root is compared too.
	if p.Issue.Host != target.Host || p.Issue.API != target.APIBase || p.Issue.Repo != target.Repo {
		a.Action, a.Detail = "skipped", fmt.Sprintf("issue %s is not on the forge the contract names now; left parked", p.Issue.URL)
		return a
	}
	state, labels, err := client.IssueState(ctx, target.Repo, p.Issue.Number)
	if err != nil {
		a.Action, a.Detail = "skipped", "read issue "+p.Issue.URL+": "+err.Error()
		return a
	}
	if state != forge.StateClosed {
		return Applied{}
	}
	if !slices.Contains(labels, ResolvedLabel) {
		a.Action, a.Detail = "skipped", "issue "+p.Issue.URL+" is closed without the "+ResolvedLabel+" label; left parked"
		return a
	}
	if err := store.Unpark(ctx, p.IntakeID); err != nil {
		a.Action, a.Detail = "skipped", "un-park: "+err.Error()
		return a
	}
	a.Action, a.Detail = "unparked", "issue "+p.Issue.URL+" closed as resolved; parked marker removed"
	return a
}

// forgeSpec returns the collection's update contract when it names a
// forge (a merge-request contract with a repo), else nil.
func forgeSpec(reg *collections.Registry, name string) *contentsource.UpdateSpec {
	c, err := reg.Get(name)
	if err != nil {
		return nil
	}
	u := c.Source.Update
	if u.DeclaredMethod() != contentsource.UpdateMergeRequest || u.Repo == "" || u.Host == "" {
		return nil
	}
	return u
}

func issueTitle(p ParkedEntry) string {
	subject := strings.Join(strings.Fields(p.Title), " ")
	if subject == "" {
		subject = p.IntakeID
	}
	title := "needs-human: " + subject
	if r := []rune(title); len(r) > maxIssueTitle {
		title = string(r[:maxIssueTitle-1]) + "…"
	}
	return title
}

// issueMarker is the first line of every issue body: it ties the issue
// to its intake item, so a run can find an issue an interrupted run
// filed. An HTML comment does not render; the id is held to a character
// set that cannot close it.
func issueMarker(intakeID string) string {
	id := strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-') {
			return r
		}
		return '_'
	}, intakeID)
	return "<!-- meerkat:intake-id " + id + " -->"
}

// issueBody summarises the disagreement. Agent- and validator-written
// text goes into fenced blocks, or through inline() into code spans, so
// a mention or markup in it neither pings anyone nor renders.
func issueBody(p ParkedEntry, marker string) string {
	var b strings.Builder
	b.WriteString(issueMarker(p.IntakeID) + "\n")
	fmt.Fprintf(&b, "meerkat's librarian parked intake item %s for a human. Nothing is waiting on this issue.\n\n", code(p.IntakeID))
	b.WriteString("**Why it was parked**\n\n" + fenced(p.Reason) + "\n")
	fmt.Fprintf(&b, "- intake_id: %s\n- target collection: %s\n", code(p.IntakeID), code(p.Collection))
	if len(p.Attempted) > 0 {
		fmt.Fprintf(&b, "- attempted path: %s\n", code(strings.Join(p.Attempted, " → ")))
	}
	fmt.Fprintf(&b, "- parked marker: %s in the intake store\n", code(marker))
	if p.Question != "" {
		b.WriteString("\n**Initial question**\n\n" + fenced(p.Question))
	}
	if len(p.FailureReasons) > 0 {
		b.WriteString("\n**Validator failure reasons**\n\n" + fenced(strings.Join(p.FailureReasons, "\n")))
	}
	fmt.Fprintf(&b, "\nWhen this is settled, close the issue with the `%s` label: the next `mk ingest --role librarian --apply` removes the parked marker. "+
		"That does not reset the candidate's failed-validation count, so fix the candidate or its sources first, or the next failed validation parks it again and files a new issue. "+
		"Closing without `%s` leaves the item parked.\n", ResolvedLabel, ResolvedLabel)
	return b.String()
}

// code renders text as an inline code span. SECURITY: the text can be
// agent-written (the attempted path and the collection derived from it
// come from mk_report_outcome), and a code span ends at a backtick or a
// blank line, after which a mention would ping and a link would render
// in an issue filed with the operator's token. So backticks become
// apostrophes and every control character (newlines included) a space.
func code(text string) string {
	return "`" + inline(text) + "`"
}

func inline(text string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, text)
	return strings.Join(strings.Fields(clean), " ")
}

// fenced wraps text in a code fence longer than any backtick run in it.
func fenced(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "text\n" + strings.TrimSpace(text) + "\n" + fence + "\n"
}
