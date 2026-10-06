// Package intake is the raw intake store and its lifecycle (meerkat-mob
// issue H): where an agent's outside research lands (mk_report_outcome,
// issue G), where a researcher's candidate page is staged, and where an
// item is parked when validators cannot agree.
//
// Layout, under the `intake:` store's prefix (any memory.Store backend:
// local, GCS, S3):
//
//	raw/<namespace>/<yyyy-mm-dd>/<id>/page.md   what an agent deposited
//	staged/<kb>/<id>.md                         a researcher's candidate page
//	done/<id>.md                                processed marker (idempotency)
//	parked/<id>.md                              needs-human, with the reason
//	                                            and, once filed, the forge issue
//
// Identity-unique intakes (Q6): the namespace is memory.Namespace of the
// depositing identity. An agent reads and writes only its own
// namespace; a librarian reads all of them. Every key is unique and
// written create-only, so the store is single-writer-per-key on every
// provider (docs/design/object-stores.md).
package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// Stages, for keys and for meerkat_intake_items_total{stage}.
const (
	StageRaw    = "raw"
	StageStaged = "staged"
	StageDone   = "done"
	StageParked = "parked"
)

// Outcomes, for meerkat_intake_items_total{outcome}.
const (
	OutcomeWritten = "written"
	OutcomeSkipped = "skipped"
	OutcomeFailed  = "failed"
)

// Item is one raw intake object: the frontmatter mk_report_outcome
// wrote (a JSON object between --- lines) plus the body.
type Item struct {
	ID        string
	Namespace string
	Key       string
	Version   memory.Version
	// Front is the raw frontmatter.
	Front map[string]any
	Body  string

	Question     string
	Attempted    []string
	Outcome      string
	FallbackKind string
	Sources      []string
	SubmittedBy  string
	ReportedAt   time.Time
}

// Store is the intake store over a memory.Store.
type Store struct {
	s memory.Store
}

// New wraps a memory store (the `intake:` block, opened).
func New(s memory.Store) *Store {
	if s == nil {
		return nil
	}
	return &Store{s: s}
}

// Describe names the backing store.
func (st *Store) Describe() string {
	if st == nil {
		return "no intake store"
	}
	return st.s.Describe()
}

// RawKey is where a deposit by namespace lands.
func RawKey(namespace string, now time.Time, id string) string {
	if namespace == "" {
		namespace = "anonymous"
	}
	return path.Join(StageRaw, namespace, now.UTC().Format("2006-01-02"), id, "page.md")
}

// StagedKey is where a candidate page for kb lands.
func StagedKey(kb, id string) string { return path.Join(StageStaged, kb, id+".md") }

func doneKey(id string) string   { return path.Join(StageDone, id+".md") }
func parkedKey(id string) string { return path.Join(StageParked, id+".md") }

// PutRaw deposits a raw page create-only and returns its key.
func (st *Store) PutRaw(ctx context.Context, namespace string, now time.Time, id string, body []byte) (string, error) {
	if st == nil {
		return "", errors.New("no intake store configured")
	}
	key := RawKey(namespace, now, id)
	if _, err := st.s.Put(ctx, key, body, memory.CreateOnly()); err != nil {
		telemetry.Record(ctx).IntakeItem(StageRaw, OutcomeFailed)
		return "", err
	}
	telemetry.Record(ctx).IntakeItem(StageRaw, OutcomeWritten)
	return key, nil
}

// ListRaw returns the raw items, oldest first. namespace "" lists every
// namespace (the librarian's view); otherwise only that identity's.
// Items with a done marker are excluded unless includeDone.
func (st *Store) ListRaw(ctx context.Context, namespace string, includeDone bool) ([]Item, error) {
	if st == nil {
		return nil, nil
	}
	recs, err := st.s.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("list intake: %w", err)
	}
	done := map[string]bool{}
	for _, r := range recs {
		if strings.HasPrefix(r.Key, StageDone+"/") {
			done[strings.TrimSuffix(path.Base(r.Key), ".md")] = true
		}
	}
	var out []Item
	var oldest time.Time
	for _, r := range recs {
		ns, id, ok := parseRawKey(r.Key)
		if !ok || (namespace != "" && ns != namespace) {
			continue
		}
		if done[id] && !includeDone {
			continue
		}
		it, err := Parse(r.Key, r.Body)
		if err != nil {
			continue // a malformed deposit is the librarian's to inspect, not a crash
		}
		it.Version = r.Version
		it.ID, it.Namespace = id, ns
		out = append(out, it)
		if !it.ReportedAt.IsZero() && (oldest.IsZero() || it.ReportedAt.Before(oldest)) {
			oldest = it.ReportedAt
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReportedAt.Equal(out[j].ReportedAt) {
			return out[i].ReportedAt.Before(out[j].ReportedAt)
		}
		return out[i].Key < out[j].Key
	})
	if !oldest.IsZero() {
		telemetry.Record(ctx).IntakeOldest(time.Since(oldest).Seconds())
	} else {
		telemetry.Record(ctx).IntakeOldest(0)
	}
	return out, nil
}

// FindRaw returns the raw item with id in any namespace, done or not.
// Unlike ListRaw it publishes no age gauge: it is a lookup, not a
// listing of what is waiting.
func (st *Store) FindRaw(ctx context.Context, id string) (Item, bool, error) {
	if st == nil {
		return Item{}, false, nil
	}
	recs, err := st.s.Load(ctx)
	if err != nil {
		return Item{}, false, fmt.Errorf("list intake: %w", err)
	}
	for _, r := range recs {
		ns, rid, ok := parseRawKey(r.Key)
		if !ok || rid != id {
			continue
		}
		it, err := Parse(r.Key, r.Body)
		if err != nil {
			return Item{}, false, err
		}
		it.Version = r.Version
		it.ID, it.Namespace = rid, ns
		return it, true, nil
	}
	return Item{}, false, nil
}

// parseRawKey splits raw/<ns>/<date>/<id>/page.md.
func parseRawKey(key string) (namespace, id string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 5 || parts[0] != StageRaw || parts[4] != "page.md" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// Parse reads a raw page: a JSON-object frontmatter between --- lines
// (what mk_report_outcome writes), then the body.
func Parse(key string, body []byte) (Item, error) {
	text := string(body)
	if !strings.HasPrefix(text, "---\n") {
		return Item{}, fmt.Errorf("%s: no frontmatter", key)
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return Item{}, fmt.Errorf("%s: unterminated frontmatter", key)
	}
	var front map[string]any
	if err := json.Unmarshal([]byte(rest[:end]), &front); err != nil {
		return Item{}, fmt.Errorf("%s: frontmatter is not a JSON object: %w", key, err)
	}
	it := Item{Key: key, Front: front, Body: strings.TrimSpace(rest[end+5:])}
	it.ID, _ = front["id"].(string)
	it.Question, _ = front["question"].(string)
	it.Outcome, _ = front["outcome"].(string)
	it.FallbackKind, _ = front["fallback_kind"].(string)
	it.SubmittedBy, _ = front["submitted_by"].(string)
	it.Attempted = stringList(front["attempted"])
	it.Sources = stringList(front["fallback_sources"])
	if s, ok := front["reported_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			it.ReportedAt = t
		}
	}
	return it, nil
}

func stringList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// IsDone reports whether an item has a processed marker.
func (st *Store) IsDone(ctx context.Context, id string) (bool, error) {
	if st == nil {
		return false, nil
	}
	_, ok, err := st.s.Stat(ctx, doneKey(id))
	return ok, err
}

// MarkDone writes the processed marker for an item with a note (the
// staged key or the page path it became). Idempotent: a second mark is
// a no-op.
func (st *Store) MarkDone(ctx context.Context, id, note string) error {
	if st == nil {
		return nil
	}
	body := []byte("# done\n\n" + note + "\n")
	_, err := st.s.Put(ctx, doneKey(id), body, memory.CreateOnly())
	if err != nil && errors.Is(err, memory.ErrConflict) {
		return nil
	}
	if err == nil {
		telemetry.Record(ctx).IntakeItem(StageDone, OutcomeWritten)
	}
	return err
}

// PutStaged stores a candidate page for kb, create-only.
func (st *Store) PutStaged(ctx context.Context, kb, id string, body []byte) (string, error) {
	if st == nil {
		return "", errors.New("no intake store configured")
	}
	key := StagedKey(kb, id)
	if _, err := st.s.Put(ctx, key, body, memory.CreateOnly()); err != nil {
		if errors.Is(err, memory.ErrConflict) {
			telemetry.Record(ctx).IntakeItem(StageStaged, OutcomeSkipped)
			return key, nil
		}
		telemetry.Record(ctx).IntakeItem(StageStaged, OutcomeFailed)
		return "", err
	}
	telemetry.Record(ctx).IntakeItem(StageStaged, OutcomeWritten)
	return key, nil
}

// Staged is one candidate page.
type Staged struct {
	KB   string
	ID   string
	Key  string
	Body []byte
}

// ListStaged returns candidate pages, by kb then id.
func (st *Store) ListStaged(ctx context.Context) ([]Staged, error) {
	if st == nil {
		return nil, nil
	}
	recs, err := st.s.Load(ctx)
	if err != nil {
		return nil, err
	}
	var out []Staged
	for _, r := range recs {
		parts := strings.Split(r.Key, "/")
		if len(parts) != 3 || parts[0] != StageStaged {
			continue
		}
		out = append(out, Staged{KB: parts[1], ID: strings.TrimSuffix(parts[2], ".md"), Key: r.Key, Body: r.Body})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Park sets an item aside for a human with the reason: validators could
// not agree, or a question needs a person. Nothing blocks waiting;
// the signal is the marker, the metric, and the caller's log line.
func (st *Store) Park(ctx context.Context, id, reason string) error {
	if st == nil {
		return nil
	}
	_, err := st.s.Put(ctx, parkedKey(id), renderParked(quoteReason(reason), ""), memory.CreateOnly())
	if err != nil && errors.Is(err, memory.ErrConflict) {
		return nil
	}
	if err == nil {
		telemetry.Record(ctx).IntakeItem(StageParked, OutcomeWritten)
	}
	return err
}

// Parked lists parked item IDs with their reasons.
func (st *Store) Parked(ctx context.Context) (map[string]string, error) {
	if st == nil {
		return nil, nil
	}
	items, err := st.ParkedDetail(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		out[it.ID] = it.Reason
	}
	return out, nil
}

// The parked marker's layout:
//
//	# needs-human
//	<!-- meerkat:parked v2 -->
//
//	<reason>
//
//	## forge issue            (once the librarian is filing or has filed one)
//
//	url: https://github.com/owner/repo/issues/12
//	host: github
//	api: https://api.github.com
//	repo: owner/repo
//	number: 12
//
// While a filing is in flight the section holds a single line,
// `filing: <RFC 3339 time>`, so a later run knows to look for an issue
// an interrupted run may have created before it files one.
//
// The second line tells this layout from the one written before forge
// issues existed (`# needs-human`, a blank line, the reason unquoted).
// A validator's reason could put anything into that older marker, a
// `## forge issue` section included, so an older marker is read as
// reason only, never as an issue reference; recording an issue rewrites
// it in this layout with the reason quoted.
const (
	parkedHeading = "# needs-human"
	parkedFormat  = "<!-- meerkat:parked v2 -->"
	issueHeading  = "## forge issue"
	filingKey     = "filing"
)

// IssueRef is the forge issue filed for a parked item (meerkat-mob #19).
// Host, API and Repo together name the forge: the host kind alone does
// not tell github.com from a GitHub Enterprise server.
type IssueRef struct {
	URL  string `json:"url"`
	Host string `json:"host"`
	// API is the forge's REST root (forge.Target.APIBase).
	API    string `json:"api"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (r IssueRef) valid() error {
	if r.Number <= 0 || r.URL == "" || r.Host == "" || r.API == "" || r.Repo == "" {
		return errors.New("an issue reference needs a url, host, api, repo and number")
	}
	return nil
}

// ParkedItem is one parked marker, read back.
type ParkedItem struct {
	ID     string
	Reason string
	// Issue is the filed forge issue, nil until one is filed.
	Issue *IssueRef
	// Filing is when a librarian run began filing an issue it has not
	// recorded yet; zero when none is in flight.
	Filing time.Time
	// IssueErr says why the marker's issue section could not be read; a
	// human must repair or remove it. Empty when it reads, or is absent.
	IssueErr string
}

// ErrFilingInFlight is BeginFiling's answer while another run's filing
// claim is fresh.
var ErrFilingInFlight = errors.New("an issue filing for this item is already in flight")

// AlreadyFiledError is returned when a parked marker already records an
// issue: the first filing wins.
type AlreadyFiledError struct{ Ref IssueRef }

func (e *AlreadyFiledError) Error() string {
	return "an issue is already recorded for this item: " + e.Ref.URL
}

// quoteReason keeps a reason from forging the issue section: a line
// that would read as the heading is indented, which Markdown and
// parseParked both treat as plain text. A reason is a validator's
// free text, and the section it could forge decides what the librarian
// queries and whether it un-parks.
func quoteReason(reason string) string {
	lines := strings.Split(reason, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines[i] = "    " + l
		}
	}
	return strings.Join(lines, "\n")
}

// renderParked writes a marker in the current layout. reasonBlock is
// already quoted; section is the issue section's lines, or "".
func renderParked(reasonBlock, section string) []byte {
	b := parkedHeading + "\n" + parkedFormat + "\n\n" + strings.Trim(reasonBlock, "\n") + "\n"
	if section != "" {
		b += "\n" + issueHeading + "\n\n" + section
	}
	return []byte(b)
}

// splitParked separates a marker into its quoted reason block and its
// issue section ("" when absent). An older marker's whole text is the
// reason, quoted here.
func splitParked(body []byte) (reasonBlock, section string, hasSection bool) {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	current := parkedHeading + "\n" + parkedFormat + "\n"
	if !strings.HasPrefix(text, current) {
		return quoteReason(strings.TrimSpace(strings.TrimPrefix(text, parkedHeading+"\n"))), "", false
	}
	text = text[len(current):]
	if i := strings.Index(text, "\n"+issueHeading+"\n"); i >= 0 {
		return strings.Trim(text[:i], "\n"), text[i+len(issueHeading)+2:], true
	}
	return strings.Trim(text, "\n"), "", false
}

// parseParked reads a parked marker.
func parseParked(id string, body []byte) ParkedItem {
	reasonBlock, section, hasSection := splitParked(body)
	it := ParkedItem{ID: id, Reason: strings.TrimSpace(reasonBlock)}
	if !hasSection {
		return it
	}
	fields := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		if _, dup := fields[k]; !ok || dup {
			it.IssueErr = "unreadable line " + strconv.Quote(line)
			return it
		}
		fields[k] = strings.TrimSpace(v)
	}
	if at, ok := fields[filingKey]; ok && len(fields) == 1 {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			it.IssueErr = "unreadable filing time " + strconv.Quote(at)
			return it
		}
		it.Filing = t
		return it
	}
	n, err := strconv.Atoi(fields["number"])
	ref := IssueRef{URL: fields["url"], Host: fields["host"], API: fields["api"], Repo: fields["repo"], Number: n}
	if err != nil || len(fields) != 5 || ref.valid() != nil {
		it.IssueErr = "the " + issueHeading + " section needs exactly url, host, api, repo and number"
		return it
	}
	it.Issue = &ref
	return it
}

// ParkedDetail lists parked items with their reasons and filed issues,
// by ID.
func (st *Store) ParkedDetail(ctx context.Context) ([]ParkedItem, error) {
	if st == nil {
		return nil, nil
	}
	recs, err := st.s.Load(ctx)
	if err != nil {
		return nil, err
	}
	var out []ParkedItem
	for _, r := range recs {
		parts := strings.Split(r.Key, "/")
		if len(parts) != 2 || parts[0] != StageParked {
			continue
		}
		out = append(out, parseParked(strings.TrimSuffix(parts[1], ".md"), r.Body))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// BeginFiling claims the filing of an issue for a parked item, so two
// librarian runs do not both file one, and so a run that dies between
// filing and recording leaves a trace. It writes `filing: <now>` into the
// marker, conditioned on the version just read, and returns the time of
// an earlier claim that was never completed (zero when there was none):
// the caller looks for an issue that earlier attempt may have created
// before it files another. A claim younger than stale (another run is
// filing now) is ErrFilingInFlight, as is losing the write race; a
// marker that already records an issue is *AlreadyFiledError.
func (st *Store) BeginFiling(ctx context.Context, id string, now time.Time, stale time.Duration) (time.Time, error) {
	if st == nil {
		return time.Time{}, errors.New("no intake store configured")
	}
	r, err := st.parkedRecord(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	it := parseParked(id, r.Body)
	switch {
	case it.Issue != nil:
		return time.Time{}, &AlreadyFiledError{Ref: *it.Issue}
	case it.IssueErr != "":
		return time.Time{}, fmt.Errorf("parked %s: %s", id, it.IssueErr)
	case !it.Filing.IsZero() && now.Sub(it.Filing).Abs() < stale:
		return time.Time{}, fmt.Errorf("parked %s: %w (claimed %s)", id, ErrFilingInFlight, it.Filing.UTC().Format(time.RFC3339))
	}
	reasonBlock, _, _ := splitParked(r.Body)
	body := renderParked(reasonBlock, fmt.Sprintf("%s: %s\n", filingKey, now.UTC().Format(time.RFC3339)))
	if _, err := st.s.Put(ctx, r.Key, body, memory.UpdateFrom(r.Version)); err != nil {
		if errors.Is(err, memory.ErrConflict) {
			return time.Time{}, fmt.Errorf("parked %s: %w (the marker changed under this run)", id, ErrFilingInFlight)
		}
		return time.Time{}, fmt.Errorf("claim issue filing for parked %s: %w", id, err)
	}
	return it.Filing, nil
}

// recordAttempts bounds SetParkedIssue's re-reads after a lost write
// race.
const recordAttempts = 3

// SetParkedIssue writes the filed issue into a parked item's marker so
// no later run files it again, replacing any filing claim. The marker is
// updated from the version just read and re-read after a lost race. An
// item that already carries a different issue keeps it: the first filing
// wins, and the caller gets *AlreadyFiledError naming that issue, so the
// one it filed can be reported as a duplicate.
func (st *Store) SetParkedIssue(ctx context.Context, id string, ref IssueRef) error {
	if st == nil {
		return errors.New("no intake store configured")
	}
	if err := ref.valid(); err != nil {
		return fmt.Errorf("parked %s: %w", id, err)
	}
	section := fmt.Sprintf("url: %s\nhost: %s\napi: %s\nrepo: %s\nnumber: %d\n",
		oneLine(ref.URL), oneLine(ref.Host), oneLine(ref.API), oneLine(ref.Repo), ref.Number)
	var err error
	for range recordAttempts {
		var r memory.Record
		if r, err = st.parkedRecord(ctx, id); err != nil {
			return err
		}
		if have := parseParked(id, r.Body).Issue; have != nil {
			if have.API == ref.API && have.Repo == ref.Repo && have.Number == ref.Number {
				return nil
			}
			return &AlreadyFiledError{Ref: *have}
		}
		reasonBlock, _, _ := splitParked(r.Body)
		if _, err = st.s.Put(ctx, r.Key, renderParked(reasonBlock, section), memory.UpdateFrom(r.Version)); err == nil {
			return nil
		}
		if !errors.Is(err, memory.ErrConflict) {
			break
		}
	}
	return fmt.Errorf("record issue for parked %s: %w", id, err)
}

// parkedRecord reads one parked marker with its version.
func (st *Store) parkedRecord(ctx context.Context, id string) (memory.Record, error) {
	key := parkedKey(id)
	recs, err := st.s.Load(ctx)
	if err != nil {
		return memory.Record{}, err
	}
	for _, r := range recs {
		if r.Key == key {
			return r, nil
		}
	}
	return memory.Record{}, fmt.Errorf("parked %s: no such parked item", id)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Unpark removes a parked item's marker once its issue was resolved:
// the needs_human finding goes away, and a later Park writes a fresh
// marker (and the librarian files a new issue). It does not re-enable
// validation, which a marker never blocked, and it does not reset the
// validation_failures count a validator keeps in its working copy. It
// needs a store that can delete (memory.Deleter, every shipped
// backend); removing a marker that is already gone is not an error.
func (st *Store) Unpark(ctx context.Context, id string) error {
	if st == nil {
		return errors.New("no intake store configured")
	}
	d, ok := st.s.(memory.Deleter)
	if !ok {
		return fmt.Errorf("intake store %s cannot delete; remove %s by hand", st.s.Describe(), parkedKey(id))
	}
	return d.Delete(ctx, parkedKey(id))
}

// ParkedKey is where an item's parked marker lives, for messages that
// point a human at it.
func ParkedKey(id string) string { return parkedKey(id) }
