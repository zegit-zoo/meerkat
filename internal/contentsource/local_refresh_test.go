package contentsource

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// local_refresh_test.go pins the configuration half of meerkat-mob#25:
// a `refresh:` block is accepted on `type: local` with the same schema,
// the same bounds and the same failure policies as on an object store,
// and an omitted interval or jitter gets the local defaults.

func loadYAML(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFile)
	write(t, path, body)
	return LoadFile(path)
}

func TestConfig_LocalRefreshDefaults(t *testing.T) {
	cases := []struct {
		name         string
		yaml         string
		pick         func(Config) Source
		wantInterval time.Duration
		wantJitter   time.Duration
		wantPolicy   string
	}{
		{
			name:         "empty block: 60s, a tenth of it as jitter",
			yaml:         "collections:\n  - name: docs\n    type: local\n    path: /srv/kb\n    refresh: {}\n",
			pick:         func(c Config) Source { return c.Collections[0].Source },
			wantInterval: DefaultLocalRefreshInterval, wantJitter: 6 * time.Second,
			wantPolicy: refresh.PolicyServeLastGood,
		},
		{
			name:         "explicit interval: jitter follows it",
			yaml:         "collections:\n  - name: docs\n    type: local\n    path: /srv/kb\n    refresh: {interval: 30s}\n",
			pick:         func(c Config) Source { return c.Collections[0].Source },
			wantInterval: 30 * time.Second, wantJitter: 3 * time.Second,
			wantPolicy: refresh.PolicyServeLastGood,
		},
		{
			name:         "explicit jitter and policy are kept",
			yaml:         "collections:\n  - name: docs\n    type: local\n    path: /srv/kb\n    refresh: {interval: 2m, jitter: 1s, failure_policy: unready}\n",
			pick:         func(c Config) Source { return c.Collections[0].Source },
			wantInterval: 2 * time.Minute, wantJitter: time.Second,
			wantPolicy: refresh.PolicyUnready,
		},
		{
			name:         "the single-source content: form",
			yaml:         "content:\n  type: local\n  path: /srv/kb\n  refresh: {}\n",
			pick:         func(c Config) Source { return c.Content },
			wantInterval: DefaultLocalRefreshInterval, wantJitter: 6 * time.Second,
			wantPolicy: refresh.PolicyServeLastGood,
		},
		{
			name:         "a tree root",
			yaml:         "tree:\n  type: local\n  path: /srv/kb\n  refresh: {}\n",
			pick:         func(c Config) Source { return *c.Tree },
			wantInterval: DefaultLocalRefreshInterval, wantJitter: 6 * time.Second,
			wantPolicy: refresh.PolicyServeLastGood,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tc.yaml)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			src := tc.pick(cfg)
			if !src.Refreshable() {
				t.Fatal("a type: local source with a refresh: block is not Refreshable")
			}
			if got := src.Refresh.Every(); got != tc.wantInterval {
				t.Errorf("interval = %s, want %s", got, tc.wantInterval)
			}
			if got := src.Refresh.Jitter.Duration(); got != tc.wantJitter {
				t.Errorf("jitter = %s, want %s", got, tc.wantJitter)
			}
			if got := src.Refresh.Policy(); got != tc.wantPolicy {
				t.Errorf("policy = %s, want %s", got, tc.wantPolicy)
			}
		})
	}
}

// No block, no refresh: the MK-FRESH-09 default, unchanged.
func TestConfig_LocalWithoutRefreshIsNotRefreshable(t *testing.T) {
	cfg, err := loadYAML(t, "collections:\n  - name: docs\n    type: local\n    path: /srv/kb\n")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	src := cfg.Collections[0].Source
	if src.Refresh != nil || src.Refreshable() {
		t.Fatalf("a local source with no refresh: block became refreshable: %+v", src.Refresh)
	}
}

// The bounds an object store's block has apply to a local one too.
func TestConfig_LocalRefreshRejections(t *testing.T) {
	cases := []struct {
		name, block, wantErr string
	}{
		{"interval below the minimum", "{interval: 1s}", "below the 5s minimum"},
		{"jitter not below the interval", "{interval: 10s, jitter: 10s}", "must be smaller than"},
		{"negative jitter", "{interval: 10s, jitter: -1s}", "must not be negative"},
		{"negative interval", "{interval: -10s}", "must be positive"},
		{"unknown failure policy", "{failure_policy: explode}", "failure_policy must be one of"},
		{"interval without a unit", "{interval: 60}", "with a unit"},
		// Keys of a later part of meerkat-mob#25, and a typo: all refused,
		// never loaded as the defaults with nothing said.
		{"on_divergence before it exists", "{on_divergence: pull}", `has no key "on_divergence"`},
		{"remote_check before it exists", "{interval: 60s, remote_check: 15m}", `has no key "remote_check"`},
		{"a typo", "{intreval: 5m}", `has no key "intreval"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, "collections:\n  - name: docs\n    type: local\n    path: /srv/kb\n    refresh: "+tc.block+"\n")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// Defaulting replaces the block rather than writing through the pointer,
// so a Source copied before it ran keeps the block it was given.
func TestDefaultRefresh_DoesNotWriteThroughASharedBlock(t *testing.T) {
	shared := &refresh.Spec{}
	a := Source{Type: TypeLocal, Path: "/srv/kb", Refresh: shared}
	b := a
	b.defaultRefresh()
	if shared.Interval != 0 || shared.Jitter != 0 {
		t.Errorf("the shared block was modified: %+v", shared)
	}
	if b.Refresh.Every() != DefaultLocalRefreshInterval {
		t.Errorf("the defaulted copy has interval %s", b.Refresh.Every())
	}
	gcs := Source{Type: TypeGCS, Bucket: "b", Prefix: "p/", Refresh: &refresh.Spec{}}
	gcs.defaultRefresh()
	if gcs.Refresh.Interval != 0 {
		t.Error("an object store's refresh: block was given a default interval; it must stay required")
	}
}

// An explicit `jitter: 0s` turns the spread off; only an omitted jitter
// gets the default.
func TestConfig_LocalRefreshExplicitZeroJitterIsKept(t *testing.T) {
	cfg, err := loadYAML(t, "collections:\n  - name: docs\n    type: local\n    path: /srv/kb\n    refresh: {interval: 30s, jitter: 0s}\n")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if j := cfg.Collections[0].Refresh.Jitter.Duration(); j != 0 {
		t.Errorf("jitter = %s, want the explicit 0s kept", j)
	}
}

// The strict keys apply to every refresh: block, not only a local one: a
// typo in an object store's block used to load silently too.
func TestConfig_ObjectStoreRefreshRejectsUnknownKeys(t *testing.T) {
	_, err := loadYAML(t, "collections:\n  - name: docs\n    type: gcs\n    bucket: b\n    prefix: p/\n    refresh: {interval: 60s, failure_polcy: unready}\n")
	if err == nil || !strings.Contains(err.Error(), `has no key "failure_polcy"`) {
		t.Fatalf("err = %v, want the typo refused", err)
	}
}

// A YAML merge key is allowed in a refresh: block, as it was before keys
// were checked, and what it merges in is held to the same keys (review
// N5). A jitter: key that arrives through the merge still counts as
// written.
func TestConfig_RefreshMergeKeys(t *testing.T) {
	cfg, err := loadYAML(t, `collections:
  - name: one
    type: local
    path: /srv/one
    refresh: &base {interval: 30s, jitter: 0s}
  - name: two
    type: local
    path: /srv/two
    refresh:
      <<: *base
      failure_policy: unready
`)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	r := cfg.Collections[1].Refresh
	if r.Every() != 30*time.Second || r.Jitter.Duration() != 0 || r.Policy() != refresh.PolicyUnready {
		t.Errorf("merged block = %+v, want interval 30s, the explicit 0s jitter kept, policy unready", r)
	}

	_, err = loadYAML(t, "collections:\n  - name: docs\n    type: local\n    path: /srv/kb\n    refresh: {<<: {intreval: 5m}}\n")
	if err == nil || !strings.Contains(err.Error(), `has no key "intreval"`) {
		t.Fatalf("err = %v, want the typo inside the merge refused", err)
	}
}
