package traversal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// query_retention_test.go pins #124 (MK-A-6): the caller's initial
// query is stored only when the operator opts in, and the log expires
// after DefaultRetentionDays unless the operator says otherwise.

func openTestLog(t *testing.T, cfg Config) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	cfg.Backend, cfg.Path, cfg.HMACKeyEnv = BackendLocal, dir, "MEERKAT_TEST_PATH_KEY"
	l, err := Open(context.Background(), &cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l, dir
}

func recordedText(t *testing.T, l *Log, dir string, e Entry) string {
	t.Helper()
	key, err := l.Record(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Without `query:`, or with `query: omit` written out, nothing of the
// query reaches the sink, and the rest of the entry is unchanged.
func TestRecord_QueryIsOmittedByDefault(t *testing.T) {
	for _, mode := range []string{"", QueryOmit} {
		l, dir := openTestLog(t, Config{Query: mode})
		text := recordedText(t, l, dir, Entry{Session: "s", Outcome: "gave_up", InitialQuery: "rotate the payroll api key", Attempted: []string{"flux"}})
		if strings.Contains(text, "payroll") || strings.Contains(text, "initial_query") {
			t.Errorf("query %q: the query reached the log: %s", mode, text)
		}
		if !strings.Contains(text, `"outcome":"gave_up"`) || !strings.Contains(text, l.Hash("flux")) {
			t.Errorf("query %q: the rest of the entry was lost: %s", mode, text)
		}
		if l.KeepsQueries() {
			t.Errorf("query %q: KeepsQueries() = true", mode)
		}
	}
}

func TestRecord_QueryIsKeptWhenOptedIn(t *testing.T) {
	l, dir := openTestLog(t, Config{Query: QueryPlaintext})
	text := recordedText(t, l, dir, Entry{Session: "s", Outcome: "gave_up", InitialQuery: "rotate the payroll api key"})
	if !strings.Contains(text, `"initial_query":"rotate the payroll api key"`) {
		t.Errorf("an opted-in log dropped the query: %s", text)
	}
	if !l.KeepsQueries() {
		t.Error("KeepsQueries() = false with query: plaintext")
	}
}

// `query: omit` is the default written out, and an unknown mode is
// refused at load rather than read as either.
func TestConfig_QueryModes(t *testing.T) {
	base := Config{Backend: BackendLocal, Path: "/x", HMACKeyEnv: "K"}
	for _, q := range []string{"", QueryOmit, QueryPlaintext} {
		c := base
		c.Query = q
		if err := c.Validate("tl"); err != nil {
			t.Errorf("query %q refused: %v", q, err)
		}
	}
	c := base
	c.Query = "hashed"
	if err := c.Validate("tl"); err == nil || !strings.Contains(err.Error(), "tl.query") {
		t.Errorf("query: hashed = %v, want a refusal naming tl.query", err)
	}
}

// Unset means the default window; an explicit 0 still keeps everything,
// so a configuration that wrote 0 before this change behaves as it did.
func TestConfig_RetentionDefault(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{
		{"backend: local\npath: /x\nhmac_key_env: K\n", DefaultRetentionDays},
		{"backend: local\npath: /x\nhmac_key_env: K\nretention_days: 0\n", 0},
		{"backend: local\npath: /x\nhmac_key_env: K\nretention_days: 7\n", 7},
	} {
		var c Config
		if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
			t.Fatal(err)
		}
		if got := c.Retention(); got != tc.want {
			t.Errorf("%q: Retention() = %d, want %d", tc.yaml, got, tc.want)
		}
	}
	if DefaultRetentionDays != 90 {
		t.Errorf("DefaultRetentionDays = %d; the operator decision (2026-10-02, #124) is 90", DefaultRetentionDays)
	}
	neg := -1
	if err := (&Config{Backend: BackendLocal, Path: "/x", HMACKeyEnv: "K", RetentionDays: &neg}).Validate("tl"); err == nil {
		t.Error("a negative retention was accepted")
	}
}

// Open runs the retention pass with the default window: a day older than
// 90 days is gone at startup, a recent one stays. With retention_days: 0
// both stay.
func TestOpen_PrunesWithTheDefaultWindow(t *testing.T) {
	now := time.Now().UTC()
	old, recent := now.AddDate(0, 0, -(DefaultRetentionDays+5)).Format("2006-01-02"), now.AddDate(0, 0, -3).Format("2006-01-02")
	seed := func(t *testing.T, dir string) {
		t.Helper()
		for _, day := range []string{old, recent} {
			p := filepath.Join(dir, filepath.FromSlash(Prefix), day)
			if err := os.MkdirAll(p, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "x.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	days := func(t *testing.T, dir string) string {
		t.Helper()
		got, err := (&localSink{dir: filepath.Join(dir, filepath.FromSlash(Prefix))}).Days(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}

	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	dir := t.TempDir()
	seed(t, dir)
	if _, err := Open(context.Background(), &Config{Backend: BackendLocal, Path: dir, HMACKeyEnv: "MEERKAT_TEST_PATH_KEY"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := days(t, dir); got != recent {
		t.Errorf("after Open with the default window: days = %q, want only %q", got, recent)
	}

	keepAll := 0
	dir = t.TempDir()
	seed(t, dir)
	if _, err := Open(context.Background(), &Config{Backend: BackendLocal, Path: dir, HMACKeyEnv: "MEERKAT_TEST_PATH_KEY", RetentionDays: &keepAll}, nil); err != nil {
		t.Fatal(err)
	}
	if got := days(t, dir); got != old+","+recent {
		t.Errorf("retention_days: 0 pruned: days = %q", got)
	}
}
