package intake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/memory"
)

// StageValidations holds one record per validator run the pipeline
// executed:
//
//	validations/<id>/<run>.md   (a JSON object)
//
// A candidate's confirmations are counted from these records, never
// from the candidate page: the page is agent-written text, and what it
// says about who verified it is not evidence (meerkat-mob#33). Only
// `mk ingest --role validator` writes here, after it has run the
// validator and checked what the run changed.
const StageValidations = "validations"

// Validation outcomes.
const (
	ValidationConfirmed = "confirmed"
	ValidationFailed    = "failed"
)

// Validation is one validator run, as the pipeline recorded it.
type Validation struct {
	// Run is the pipeline-generated run id; one record per run.
	Run string `json:"run"`
	// Model is the --model the pipeline ran the validator with.
	Model   string    `json:"model"`
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
	// Reason is the validator's failure_reason when it failed.
	Reason string `json:"reason,omitempty"`
}

// NewRunID returns a fresh validator run id: the time, for ordering,
// and random bytes, so two runs in the same instant never share a key.
func NewRunID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+).
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b[:])
}

// validationKey is where one run's record lives. Both parts must be a
// single plain path segment.
func validationKey(id, run string) (string, error) {
	for _, s := range []string{id, run} {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
			return "", fmt.Errorf("intake: %q is not a single key segment", s)
		}
	}
	return path.Join(StageValidations, id, run+".md"), nil
}

// RecordValidation stores one validator run's outcome, create-only.
func (st *Store) RecordValidation(ctx context.Context, id string, v Validation) error {
	if st == nil {
		return errors.New("no intake store configured")
	}
	if v.Outcome != ValidationConfirmed && v.Outcome != ValidationFailed {
		return fmt.Errorf("intake: validation outcome %q", v.Outcome)
	}
	key, err := validationKey(id, v.Run)
	if err != nil {
		return err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = st.s.Put(ctx, key, body, memory.CreateOnly())
	return err
}

// Validations returns the recorded validator runs for an item, oldest
// first. A record that does not read is skipped: it was not written by
// RecordValidation and so is not evidence of a run.
func (st *Store) Validations(ctx context.Context, id string) ([]Validation, error) {
	if st == nil {
		return nil, nil
	}
	recs, err := st.s.Load(ctx)
	if err != nil {
		return nil, err
	}
	prefix := StageValidations + "/" + id + "/"
	var out []Validation
	for _, r := range recs {
		rest, ok := strings.CutPrefix(r.Key, prefix)
		if !ok || strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".md") {
			continue
		}
		var v Validation
		if err := json.Unmarshal(r.Body, &v); err != nil || v.Run != strings.TrimSuffix(rest, ".md") {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Run < out[j].Run
	})
	return out, nil
}

// ConfirmingModels returns the distinct models whose recorded runs
// confirmed the item, in first-confirmed order, leaving out exclude
// (the researcher's model, which may not validate its own work).
// Two runs of one model count once: independence is per model.
func ConfirmingModels(vals []Validation, exclude string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		if v.Outcome != ValidationConfirmed || v.Model == "" || v.Model == exclude || seen[v.Model] {
			continue
		}
		seen[v.Model] = true
		out = append(out, v.Model)
	}
	return out
}

// Failures returns the recorded failed runs.
func Failures(vals []Validation) []Validation {
	var out []Validation
	for _, v := range vals {
		if v.Outcome == ValidationFailed {
			out = append(out, v)
		}
	}
	return out
}
