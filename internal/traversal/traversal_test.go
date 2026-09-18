package traversal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil", nil, ""},
		{"local ok", &Config{Backend: "local", Path: "/var/lib/meerkat/paths", HMACKeyEnv: "K"}, ""},
		{"s3 ok", &Config{Backend: "s3", Bucket: "kb", Prefix: "tele/", Endpoint: "https://s3.example.net", HMACKeyEnv: "K"}, ""},
		{"no backend", &Config{HMACKeyEnv: "K"}, "backend is required"},
		{"bad backend", &Config{Backend: "gcs", HMACKeyEnv: "K"}, "backend must be"},
		{"local no path", &Config{Backend: "local", HMACKeyEnv: "K"}, "path is required"},
		{"local with bucket", &Config{Backend: "local", Path: "/x", Bucket: "b", HMACKeyEnv: "K"}, "apply to backend: s3"},
		{"s3 no bucket", &Config{Backend: "s3", HMACKeyEnv: "K"}, "bucket is required"},
		{"s3 bad endpoint", &Config{Backend: "s3", Bucket: "b", Endpoint: "s3.example.net", HMACKeyEnv: "K"}, "http(s)://"},
		{"no key env", &Config{Backend: "local", Path: "/x"}, "hmac_key_env is required"},
		{"negative retention", &Config{Backend: "local", Path: "/x", HMACKeyEnv: "K", RetentionDays: -1}, "retention_days"},
	}
	for _, c := range cases {
		err := c.cfg.Validate("observability.traversal_log")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestOpen_KeyComesFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Backend: "local", Path: dir, HMACKeyEnv: "MEERKAT_TEST_PATH_KEY"}
	t.Setenv("MEERKAT_TEST_PATH_KEY", "")
	if _, err := Open(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "MEERKAT_TEST_PATH_KEY") {
		t.Errorf("unset key: err = %v", err)
	}
	t.Setenv("MEERKAT_TEST_PATH_KEY", "short")
	if _, err := Open(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "at least 16") {
		t.Errorf("short key: err = %v", err)
	}
	t.Setenv("MEERKAT_TEST_PATH_KEY", "a-key-that-is-long-enough-for-hmac")
	l, err := Open(context.Background(), cfg, nil)
	if err != nil || l == nil || !l.Enabled() {
		t.Fatalf("Open: %v", err)
	}
	var nilLog *Log
	if nilLog.Enabled() || nilLog.Hash("x") != "" {
		t.Error("a nil log must be a no-op")
	}
	if k, err := nilLog.Record(context.Background(), Entry{}); err != nil || k != "" {
		t.Error("a nil log must record nothing")
	}
	if n, err := Open(context.Background(), nil, nil); err != nil || n != nil {
		t.Error("nil config yields a nil log")
	}
}

func TestRecord_HashesIdentityKeepsShapeAndQuery(t *testing.T) {
	dir := t.TempDir()
	sink := &localSink{dir: filepath.Join(dir, "telemetry", "paths")}
	l := NewLog(sink, []byte("not a secret, a test key long enough"), 0)
	fixed := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return fixed }

	key, err := l.Record(context.Background(), Entry{
		Session: "sess-1", Outcome: "gave_up", InitialQuery: "datadgo monitor serach",
		Pages: []string{"flux:concepts/drift"}, Attempted: []string{"root", "platform", "flux"},
		PathShape: []int{0, 1, 2}, TierReached: 2,
		Quality:  &Quality{Accuracy: 0.5, Completeness: 0.25, AnswerQuality: 0.75},
		Fallback: &Fallback{Kind: "web", Summary: "found it on the vendor site", Sources: []string{"https://example.com/doc"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "telemetry/paths/2026-09-18/") || !strings.HasSuffix(key, ".json") {
		t.Errorf("key = %q", key)
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, plain := range []string{"sess-1", "flux:concepts/drift", `"root"`, `"platform"`, `"flux"`} {
		if strings.Contains(text, plain) {
			t.Errorf("plaintext identity %q leaked into the log: %s", plain, text)
		}
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.Version != 1 || e.InitialQuery != "datadgo monitor serach" || e.Hops != 3 || e.TierReached != 2 || len(e.PathShape) != 3 {
		t.Errorf("entry = %+v", e)
	}
	if e.Session != l.Hash("sess-1") || e.Pages[0] != l.Hash("flux:concepts/drift") || e.Attempted[2] != l.Hash("flux") {
		t.Errorf("hashes must be the HMAC of the plaintext under the key: %+v", e)
	}
	if len(e.Session) != 64 {
		t.Errorf("hash = %q, want 64 hex chars", e.Session)
	}
	other := NewLog(sink, []byte("another-key-another-key-another"), 0)
	if other.Hash("flux") == l.Hash("flux") {
		t.Error("different keys must hash differently")
	}
	if e.Fallback == nil || e.Fallback.Kind != "web" || e.Quality == nil || e.Quality.AnswerQuality != 0.75 {
		t.Errorf("fallback/quality lost: %+v", e)
	}
}

func TestPrune_RetentionIsAnApplicationJob(t *testing.T) {
	dir := t.TempDir()
	sink := &localSink{dir: dir}
	for _, day := range []string{"2026-09-01", "2026-09-10", "2026-09-18", "not-a-day"} {
		if err := os.MkdirAll(filepath.Join(dir, day), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, day, "x.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := NewLog(sink, []byte("not a secret, a test key long enough"), 7)
	l.now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	if err := l.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	days, _ := sink.Days(context.Background())
	if strings.Join(days, ",") != "2026-09-18" {
		t.Errorf("days after prune = %v, want only 2026-09-18 (7-day window from the 18th keeps the 11th on)", days)
	}
	if _, err := os.Stat(filepath.Join(dir, "not-a-day")); err != nil {
		t.Error("a non-day directory must never be deleted")
	}
	unbounded := NewLog(sink, []byte("not a secret, a test key long enough"), 0)
	if err := unbounded.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.DeleteDay(context.Background(), "../escape"); err == nil {
		t.Error("DeleteDay must refuse a non-day argument")
	}
}
