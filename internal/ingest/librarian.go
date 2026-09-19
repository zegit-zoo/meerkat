package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// librarian.go is the librarian role (meerkat-mob issue H): the run that
// keeps the whole tree honest. It loads every collection the registry
// holds (cold ones included), and reports
//
//   - dangling `related:` entries and pointer targets (issue B's link
//     graph),
//   - pages past `stale_after`,
//   - cull proposals: pages stale for longer than CullAfter (archive,
//     never delete — Q8),
//   - missing links: collections agents keep trying and giving up in,
//     read from the traversal log (issue G) and matched by hashed name,
//   - parked intake items that need a human,
//   - staged candidates with enough confirmations to file,
//   - promotions: hot deep collections that deserve a pointer from the
//     root (promotion.go).
//
// Nothing is modified unless Apply is called, and Apply files only what
// the collection's update contract allows: `direct` writes the page
// into the collection's memory store; `merge-request` and `none` are
// reported with the instructions, not acted on.

// CullAfter is how long past stale_after a page must be before the
// librarian proposes archiving it.
const CullAfter = 90 * 24 * time.Hour

// Finding kinds, also the meerkat_librarian_findings_total{kind} label.
const (
	FindingDangling    = "dangling"
	FindingStale       = "stale"
	FindingCull        = "cull"
	FindingMissingLink = "missing_link"
	FindingNeedsHuman  = "needs_human"
)

// Finding is one thing the librarian noticed.
type Finding struct {
	Kind       string `json:"kind"`
	Collection string `json:"collection,omitempty"`
	Page       string `json:"page,omitempty"`
	Detail     string `json:"detail"`
	// Count is how many sessions supported a missing-link finding.
	Count int `json:"count,omitempty"`
}

// Report is a librarian run's output.
type Report struct {
	At       time.Time `json:"at"`
	Findings []Finding `json:"findings"`
	// Fileable lists staged candidates with the required confirmations,
	// by target collection and contract method.
	Fileable []Fileable `json:"fileable,omitempty"`
	// Promotions lists hot deep collections proposed for a root pointer,
	// hottest first. Each also appears in Findings as a promotion.
	Promotions []Promotion `json:"promotions,omitempty"`
}

// Fileable is a confirmed candidate and how its collection takes it.
type Fileable struct {
	IntakeID   string `json:"intake_id"`
	Collection string `json:"collection"`
	Method     string `json:"method"`
	Key        string `json:"staged_key"`
	PageID     string `json:"page_id"`
}

// Count returns how many findings of a kind the report has.
func (r Report) Count(kind string) int {
	n := 0
	for _, f := range r.Findings {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// LibrarianOpts configures a run.
type LibrarianOpts struct {
	// Log is the traversal log for missing-link analysis; nil skips it.
	Log *traversal.Log
	// Days of traversal log to read.
	Days int
	// MinGiveUps is how many gave_up/not_found sessions that tried a
	// collection make a missing-link finding; default 3.
	MinGiveUps int
	// PromotionTopN caps promotion proposals per run; default 5.
	// PromotionMinDepth is the shallowest tree depth that counts as
	// deep; default 2.
	PromotionTopN     int
	PromotionMinDepth int
	Now               func() time.Time
}

// Librarian inspects the registry and the intake store and reports.
// It changes nothing.
func Librarian(ctx context.Context, reg *collections.Registry, store *intake.Store, opts LibrarianOpts) (*Report, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MinGiveUps <= 0 {
		opts.MinGiveUps = 3
	}
	if opts.PromotionTopN <= 0 {
		opts.PromotionTopN = DefaultPromotionTopN
	}
	if opts.PromotionMinDepth <= 0 {
		opts.PromotionMinDepth = DefaultPromotionMinDepth
	}
	now := opts.Now()
	rep := &Report{At: now}
	m := telemetry.Record(ctx)

	// Links.
	for _, d := range reg.LinkReport().Dangling {
		rep.Findings = append(rep.Findings, Finding{Kind: FindingDangling, Collection: d.Collection, Page: d.PageID, Detail: d.String()})
		m.LibrarianFinding(FindingDangling)
	}

	// Stale and cull. Every collection, cold ones mounted for the run.
	for _, c := range reg.All() {
		pages, err := reg.Pages(c.Name)
		if err != nil {
			rep.Findings = append(rep.Findings, Finding{Kind: FindingStale, Collection: c.Name, Detail: "could not list: " + err.Error()})
			continue
		}
		for _, ref := range pages {
			p := ref.Page
			if !p.Front.IsStale(now) {
				continue
			}
			staleSince, _ := time.Parse("2006-01-02", p.Front.StaleAfter)
			if now.Sub(staleSince) > CullAfter && !isConfirmedRecently(p, now) {
				rep.Findings = append(rep.Findings, Finding{Kind: FindingCull, Collection: c.Name, Page: p.ID, Detail: fmt.Sprintf("stale since %s (%d days); propose archiving to _archive/ through the %s contract", p.Front.StaleAfter, int(now.Sub(staleSince).Hours()/24), contractMethod(c))})
				m.LibrarianFinding(FindingCull)
				continue
			}
			rep.Findings = append(rep.Findings, Finding{Kind: FindingStale, Collection: c.Name, Page: p.ID, Detail: "past stale_after " + p.Front.StaleAfter + "; refresh"})
			m.LibrarianFinding(FindingStale)
		}
	}

	// Missing links from the traversal log: collections that sessions
	// tried and gave up in. Hashed names are matched by hashing the
	// registry's own names with the log's key.
	if opts.Log != nil && opts.Days > 0 {
		counts, err := giveUpsByCollection(ctx, reg, opts.Log, opts.Days)
		if err != nil {
			return nil, err
		}
		var names []string
		for n := range counts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if counts[n] >= opts.MinGiveUps {
				rep.Findings = append(rep.Findings, Finding{Kind: FindingMissingLink, Collection: n, Count: counts[n],
					Detail: fmt.Sprintf("%d sessions searched here and gave up in the last %d days; propose a pointer or related: entry for what they asked (initial queries are in the traversal log)", counts[n], opts.Days)})
				m.LibrarianFinding(FindingMissingLink)
			}
		}
		// Promotions: hot deep collections without a root pointer.
		proms, err := promotions(ctx, reg, opts.Log, opts)
		if err != nil {
			return nil, err
		}
		for _, p := range proms {
			rep.Promotions = append(rep.Promotions, p)
			rep.Findings = append(rep.Findings, Finding{Kind: FindingPromotion, Collection: p.Hub, Page: p.PageID(), Count: int(min(p.Temperature, 1<<30)), Detail: p.detail()})
			m.LibrarianFinding(FindingPromotion)
		}
	}

	// Intake: parked items and fileable candidates.
	if store != nil {
		parked, err := store.Parked(ctx)
		if err != nil {
			return nil, err
		}
		var ids []string
		for id := range parked {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rep.Findings = append(rep.Findings, Finding{Kind: FindingNeedsHuman, Page: id, Detail: parked[id]})
			m.LibrarianFinding(FindingNeedsHuman)
		}
		staged, err := store.ListStaged(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range staged {
			if done, _ := store.IsDone(ctx, "filed-"+s.ID); done {
				continue
			}
			p, err := kb.ParsePage(s.ID, s.Key, s.Body)
			if err != nil || !Confirmed(p) {
				continue
			}
			method := contractMethodByName(reg, s.KB)
			rep.Fileable = append(rep.Fileable, Fileable{IntakeID: s.ID, Collection: s.KB, Method: method, Key: s.Key, PageID: p.ID})
		}
	}
	return rep, nil
}

func isConfirmedRecently(p kb.Page, now time.Time) bool {
	for _, v := range p.Front.Verified {
		if t, err := time.Parse(time.RFC3339, v.At); err == nil && now.Sub(t) < CullAfter {
			return true
		}
	}
	return false
}

func contractMethod(c *collections.Collection) string {
	if c == nil || !c.Contract().Declared() {
		return contentsource.UpdateNone
	}
	return string(c.Contract().Method)
}

func contractMethodByName(reg *collections.Registry, name string) string {
	c, err := reg.Get(name)
	if err != nil {
		return contentsource.UpdateNone
	}
	return contractMethod(c)
}

// giveUpsByCollection counts, per mounted collection, the sessions in
// the last days that attempted it and ended gave_up or not_found.
func giveUpsByCollection(ctx context.Context, reg *collections.Registry, log *traversal.Log, days int) (map[string]int, error) {
	entries, err := log.ReadSessions(ctx, days)
	if err != nil {
		return nil, err
	}
	byHash := map[string]string{}
	for _, name := range reg.Names() {
		byHash[log.Hash(name)] = name
	}
	counts := map[string]int{}
	for _, e := range entries {
		if e.Outcome != "gave_up" && e.Outcome != "not_found" {
			continue
		}
		seen := map[string]bool{}
		for _, h := range e.Attempted {
			if name, ok := byHash[h]; ok && !seen[name] {
				counts[name]++
				seen[name] = true
			}
		}
	}
	return counts, nil
}

// Write renders a report as text.
func (r Report) Write(w io.Writer) {
	for _, f := range r.Findings {
		where := f.Collection
		if f.Page != "" {
			if where != "" {
				where += ":"
			}
			where += f.Page
		}
		fmt.Fprintf(w, "%-13s %-40s %s\n", f.Kind, where, f.Detail)
	}
	for _, f := range r.Fileable {
		fmt.Fprintf(w, "%-13s %-40s confirmed candidate %s -> %s via %s\n", "fileable", f.Collection, f.IntakeID, f.PageID, f.Method)
	}
}

// Applied is what Apply did with one fileable candidate.
type Applied struct {
	IntakeID string
	Action   string // filed | instructions | skipped
	Detail   string
}

// Apply files the report's confirmed candidates through their
// collection's contract: `direct` writes the page into the collection's
// memory store (global scope) and marks the intake item filed;
// `merge-request` and `none` produce instructions and change nothing.
// Promotion proposals are filed the same way into the root hub (see
// applyPromotions); their Applied.IntakeID is "promote:<collection>".
func Apply(ctx context.Context, reg *collections.Registry, store *intake.Store, rep *Report) ([]Applied, error) {
	out, err := applyPromotions(ctx, reg, rep)
	if err != nil {
		return out, err
	}
	for _, f := range rep.Fileable {
		a := Applied{IntakeID: f.IntakeID}
		c, err := reg.Get(f.Collection)
		if err != nil {
			a.Action, a.Detail = "skipped", err.Error()
			out = append(out, a)
			continue
		}
		switch f.Method {
		case contentsource.UpdateDirect:
			st := c.Memory()
			if st == nil {
				a.Action, a.Detail = "skipped", "contract is direct but the collection has no memory store"
				out = append(out, a)
				continue
			}
			body, err := stagedBody(ctx, store, f)
			if err != nil {
				return out, err
			}
			key := "global/intake/" + f.IntakeID + ".md"
			if _, err := st.Put(ctx, key, body, memory.CreateOnly()); err != nil && !errors.Is(err, memory.ErrConflict) {
				return out, fmt.Errorf("file %s into %q: %w", f.IntakeID, f.Collection, err)
			}
			if err := store.MarkDone(ctx, "filed-"+f.IntakeID, st.Location(key)); err != nil {
				return out, err
			}
			a.Action, a.Detail = "filed", st.Location(key)
		case contentsource.UpdateMergeRequest:
			ec := c.Contract()
			a.Action, a.Detail = "instructions", fmt.Sprintf("open a merge request on %s (%s, branch %s, path %s) adding %s; %s", ec.Repo, ec.Host, ec.Branch, ec.Path, f.PageID, strings.TrimSpace(ec.Instructions))
		default:
			a.Action, a.Detail = "instructions", "collection "+f.Collection+" declares no contribution path; a human must place "+f.PageID
		}
		out = append(out, a)
	}
	return out, nil
}

func stagedBody(ctx context.Context, store *intake.Store, f Fileable) ([]byte, error) {
	staged, err := store.ListStaged(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range staged {
		if s.Key == f.Key {
			return s.Body, nil
		}
	}
	return nil, fmt.Errorf("staged candidate %s vanished", f.Key)
}
