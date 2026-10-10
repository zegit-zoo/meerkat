package traversal_test

import (
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/intake"
)

// TestIntakeLayoutIsWhatRecordStrips pins traversal's copy of the intake
// raw-key layout to intake.RawKey: if RawKey changes shape, Record would
// stop removing the namespace from intake_id (meerkat-mob#53).
func TestIntakeLayoutIsWhatRecordStrips(t *testing.T) {
	key := intake.RawKey("ns", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), "id")
	if key != "raw/ns/2026-10-07/id/page.md" {
		t.Errorf("intake.RawKey = %q; update stripIntakeNamespace in traversal.go to match", key)
	}
}
