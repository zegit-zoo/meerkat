package traversal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// dailycap_test.go pins meerkat-mob#45's daily write cap: Record refuses
// past either cap until the UTC day turns over, a failed write does not
// use up room, and the configuration's defaults and bounds hold.

type failingPut struct {
	*memSink
	fail bool
}

func (f *failingPut) Put(ctx context.Context, key string, body []byte) error {
	if f.fail {
		return errors.New("put denied")
	}
	return f.memSink.Put(ctx, key, body)
}

func TestRecord_DailyEntryCap(t *testing.T) {
	now := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
	sink := &failingPut{memSink: newMemSink()}
	l := NewLog(sink, testKey, 0)
	l.now = func() time.Time { return now }
	l.maxEntries = 2
	ctx := context.Background()

	// A failed write gives its room back.
	sink.fail = true
	if _, err := l.Record(ctx, Entry{Session: "s", Outcome: "found"}); err == nil || errors.Is(err, ErrDailyCap) {
		t.Fatalf("failed put: %v", err)
	}
	sink.fail = false
	for i := range 2 {
		if _, err := l.Record(ctx, Entry{Session: "s", Outcome: "found"}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := l.Record(ctx, Entry{Session: "s", Outcome: "found"}); !errors.Is(err, ErrDailyCap) {
		t.Fatalf("third write = %v, want ErrDailyCap", err)
	}
	if n := len(sink.objects["2026-10-16"]); n != 2 {
		t.Errorf("%d objects written, want 2", n)
	}
	if n := l.CappedWrites(); n != 1 {
		t.Errorf("CappedWrites = %d, want 1", n)
	}

	// The next UTC day starts afresh.
	now = now.Add(2 * time.Hour)
	if _, err := l.Record(ctx, Entry{Session: "s", Outcome: "found"}); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestRecord_DailyByteCap(t *testing.T) {
	sink := newMemSink()
	l := NewLog(sink, testKey, 0)
	ctx := context.Background()
	if _, err := l.Record(ctx, Entry{Session: "s", Outcome: "found"}); err != nil {
		t.Fatal(err)
	}
	var one int64
	for _, objs := range sink.objects {
		for _, b := range objs {
			one = int64(len(b))
		}
	}
	// Room for this entry and a bit, not two.
	l2 := NewLog(newMemSink(), testKey, 0)
	l2.maxBytes = one + one/2
	if _, err := l2.Record(ctx, Entry{Session: "s", Outcome: "found"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.Record(ctx, Entry{Session: "s", Outcome: "found"}); !errors.Is(err, ErrDailyCap) {
		t.Fatalf("second write = %v, want ErrDailyCap", err)
	}
}

func TestRecordTemperatures_NotCapped(t *testing.T) {
	l := NewLog(newMemSink(), testKey, 0)
	l.maxEntries = 1
	ctx := context.Background()
	if _, err := l.Record(ctx, Entry{Session: "s", Outcome: "found"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordTemperatures(ctx, map[string]Temperature{"notes": {Temperature: 1}}); err != nil {
		t.Fatalf("temperature flush after the cap: %v", err)
	}
}

func TestConfig_DailyCaps(t *testing.T) {
	cfg := &Config{Backend: BackendLocal, Path: "/x", HMACKeyEnv: "K"}
	if e, b := cfg.DailyCaps(); e != DefaultMaxEntriesPerDay || b != DefaultMaxBytesPerDay {
		t.Errorf("defaults = %d, %d", e, b)
	}
	zero, zero64 := 0, int64(0)
	cfg.MaxEntriesPerDay, cfg.MaxBytesPerDay = &zero, &zero64
	if e, b := cfg.DailyCaps(); e != 0 || b != 0 {
		t.Errorf("explicit 0 = %d, %d, want no cap", e, b)
	}
	if err := cfg.Validate("t"); err != nil {
		t.Errorf("0 caps rejected: %v", err)
	}
	neg, neg64 := -1, int64(-1)
	cfg.MaxEntriesPerDay = &neg
	if err := cfg.Validate("t"); err == nil || !strings.Contains(err.Error(), "max_entries_per_day") {
		t.Errorf("negative entry cap: %v", err)
	}
	cfg.MaxEntriesPerDay, cfg.MaxBytesPerDay = nil, &neg64
	if err := cfg.Validate("t"); err == nil || !strings.Contains(err.Error(), "max_bytes_per_day") {
		t.Errorf("negative byte cap: %v", err)
	}
}

func TestOpen_AppliesDailyCaps(t *testing.T) {
	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	one := 1
	l, err := Open(context.Background(), &Config{Backend: BackendLocal, Path: t.TempDir(), HMACKeyEnv: "MEERKAT_TEST_PATH_KEY", MaxEntriesPerDay: &one}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.maxEntries != 1 || l.maxBytes != DefaultMaxBytesPerDay {
		t.Errorf("caps = %d, %d", l.maxEntries, l.maxBytes)
	}
}
