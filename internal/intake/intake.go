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
	body := []byte("# needs-human\n\n" + reason + "\n")
	_, err := st.s.Put(ctx, parkedKey(id), body, memory.CreateOnly())
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
	recs, err := st.s.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range recs {
		if strings.HasPrefix(r.Key, StageParked+"/") {
			out[strings.TrimSuffix(path.Base(r.Key), ".md")] = strings.TrimSpace(strings.TrimPrefix(string(r.Body), "# needs-human\n"))
		}
	}
	return out, nil
}
