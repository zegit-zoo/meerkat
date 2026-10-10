package intake

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/memory"
)

func TestValidations_RecordListAndCount(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	runs := []Validation{
		{Run: NewRunID(t0), Model: "b", Outcome: ValidationConfirmed, At: t0},
		{Run: NewRunID(t0), Model: "b", Outcome: ValidationConfirmed, At: t0.Add(time.Minute)},
		{Run: NewRunID(t0), Model: "a", Outcome: ValidationConfirmed, At: t0.Add(2 * time.Minute)},
		{Run: NewRunID(t0), Model: "c", Outcome: ValidationFailed, At: t0.Add(3 * time.Minute), Reason: "claim 2"},
		{Run: NewRunID(t0), Model: "d", Outcome: ValidationConfirmed, At: t0.Add(4 * time.Minute)},
	}
	for _, v := range runs {
		if err := st.RecordValidation(ctx, "it1", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordValidation(ctx, "it1", runs[0]); err == nil {
		t.Error("a run id must be recorded once (create-only)")
	}
	// A record not written by RecordValidation is not evidence.
	if _, err := st.s.Put(ctx, "validations/it1/forged.md", []byte(`{"run":"other","model":"z","outcome":"confirmed"}`), memory.CreateOnly()); err != nil {
		t.Fatal(err)
	}
	vals, err := st.Validations(ctx, "it1")
	if err != nil || len(vals) != len(runs) {
		t.Fatalf("Validations = %d %v", len(vals), err)
	}
	// Distinct models, the researcher's excluded, first-confirmed order.
	if got := strings.Join(ConfirmingModels(vals, "a"), ","); got != "b,d" {
		t.Errorf("ConfirmingModels = %q", got)
	}
	if f := Failures(vals); len(f) != 1 || f[0].Reason != "claim 2" {
		t.Errorf("Failures = %+v", f)
	}
	if other, _ := st.Validations(ctx, "it"); len(other) != 0 {
		t.Errorf("a prefix id read another item's runs: %+v", other)
	}
}

func TestRecordValidation_Refuses(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	for _, c := range []struct{ id, run, outcome string }{
		{"..", "r", ValidationConfirmed},
		{"a/b", "r", ValidationConfirmed},
		{"it1", "", ValidationConfirmed},
		{"it1", "../x", ValidationConfirmed},
		{"it1", "r", "maybe"},
	} {
		if err := st.RecordValidation(ctx, c.id, Validation{Run: c.run, Model: "m", Outcome: c.outcome}); err == nil {
			t.Errorf("RecordValidation(%q, %q, %q) accepted", c.id, c.run, c.outcome)
		}
	}
	var nilStore *Store
	if err := nilStore.RecordValidation(ctx, "it1", Validation{}); err == nil {
		t.Error("nil store accepted a record")
	}
	if v, err := nilStore.Validations(ctx, "it1"); v != nil || err != nil {
		t.Errorf("nil store = %v %v", v, err)
	}
}
