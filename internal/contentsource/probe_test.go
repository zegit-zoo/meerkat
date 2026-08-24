package contentsource

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// probe_test.go drives the CHEAP half of runtime reconciliation against
// the same in-memory fake FetchGCS is tested with. The property under
// test throughout is agreement: the probe's answer and the version
// FetchGCS resolves to must be the same string, because the whole
// reconciliation decision is "are those two equal?".

func minuteRefresh() *refresh.Spec {
	return &refresh.Spec{Interval: refresh.Duration(time.Minute)}
}

// --- object mode -------------------------------------------------------

func TestGCSVersion_ObjectAgreesWithFetchAndTracksGenerations(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "---\nid: a\n---\nv1\n"}))
	src := Source{Type: TypeGCS, Bucket: "b", Object: "kb.tar.gz", Layout: defaultLayout(), Refresh: minuteRefresh()}

	probed, err := GCSVersion(context.Background(), src)
	if err != nil {
		t.Fatalf("GCSVersion: %v", err)
	}
	_, fetched, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS: %v", err)
	}
	if probed != fetched {
		t.Fatalf("probe = %q but fetch resolved %q — reconciliation compares these two", probed, fetched)
	}
	// The probe downloads nothing: only the fetch above did.
	if len(fake.reads) != 1 {
		t.Errorf("reads = %v, want only the fetch's — a probe is metadata-only", fake.reads)
	}

	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "---\nid: a\n---\nv2\n"}))
	next, err := GCSVersion(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if next == probed {
		t.Error("a new object generation did not change the probed version")
	}
}

// TestGCSVersion_PinnedGenerationMakesNoCallAtAll: a pinned source can
// never move, so asking the bucket would be a metadata request whose
// answer is already known — and, more importantly, a pinned source is
// never polled in the first place (see Refreshable).
func TestGCSVersion_PinnedGenerationMakesNoCallAtAll(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	pinned := fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "x"}))
	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "y"}))

	src := Source{Type: TypeGCS, Bucket: "b", Object: "kb.tar.gz", Generation: pinned, Layout: defaultLayout()}
	got, err := GCSVersion(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1" {
		t.Errorf("version = %q, want the pinned generation", got)
	}
	if fake.attrCalls != 0 {
		t.Errorf("attrCalls = %d, want 0 for a pinned source", fake.attrCalls)
	}
	if src.Refreshable() {
		t.Error("a pinned source must never be reported as refreshable")
	}
}

func TestGCSVersion_MissingObjectIsAnError(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	src := Source{Type: TypeGCS, Bucket: "b", Object: "gone.tar.gz", Layout: defaultLayout()}
	if _, err := GCSVersion(context.Background(), src); err == nil {
		t.Fatal("expected an error for an object that does not exist")
	}
}

// --- prefix mode -------------------------------------------------------

// TestGCSVersion_PrefixTracksAddOverwriteAndDelete is the prefix-mode
// change-detection contract: all three mutations move the fingerprint,
// and the probe agrees with the fetch at every step.
func TestGCSVersion_PrefixTracksAddOverwriteAndDelete(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb/wiki/a.md", []byte("---\nid: a\n---\nv1\n"))
	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/", Layout: defaultLayout(), Refresh: minuteRefresh()}

	agree := func(step string) string {
		t.Helper()
		probed, err := GCSVersion(context.Background(), src)
		if err != nil {
			t.Fatalf("%s: GCSVersion: %v", step, err)
		}
		_, fetched, err := FetchGCS(context.Background(), src)
		if err != nil {
			t.Fatalf("%s: FetchGCS: %v", step, err)
		}
		if probed != fetched {
			t.Fatalf("%s: probe = %q, fetch = %q", step, probed, fetched)
		}
		return probed
	}

	v1 := agree("initial")
	fake.put("kb/wiki/a.md", []byte("---\nid: a\n---\nv2\n"))
	v2 := agree("overwrite")
	if v2 == v1 {
		t.Error("an overwrite did not move the fingerprint")
	}
	fake.put("kb/wiki/b.md", []byte("---\nid: b\n---\nnew\n"))
	v3 := agree("add")
	if v3 == v2 {
		t.Error("an added object did not move the fingerprint")
	}
	fake.remove("kb/wiki/b.md")
	v4 := agree("delete")
	if v4 == v3 {
		t.Error("a deleted object did not move the fingerprint")
	}
	if v4 != v2 {
		t.Error("deleting the added object should return the fingerprint to its pre-add value")
	}
}

// TestGCSVersion_PrefixIsQuietAboutUnsafeNames: the fetch warns once
// about an object it skips; the probe, which re-lists forever, must not
// warn every interval for the life of the process.
func TestGCSVersion_PrefixIsQuietAboutUnsafeNames(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb/wiki/ok.md", []byte("---\nid: ok\n---\nfine\n"))
	fake.put("kb/../escape.md", []byte("nope"))
	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/", Layout: defaultLayout()}

	stderr := captureStderr(t, func() {
		if _, err := GCSVersion(context.Background(), src); err != nil {
			t.Fatal(err)
		}
	})
	if stderr != "" {
		t.Errorf("the probe wrote to stderr: %q", stderr)
	}
	// The fetch still warns, once, so an operator does learn about it.
	stderr = captureStderr(t, func() {
		if _, _, err := FetchGCS(context.Background(), src); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "unsafe name") {
		t.Errorf("the fetch should still warn about a skipped object, got %q", stderr)
	}
}

// TestGCSVersion_PrefixHonoursTheObjectCountCap: the probe is bound by
// the same source limit the fetch is, so a mistyped prefix fails at the
// cheap step rather than after a listing of the whole bucket has been
// hashed every minute forever.
func TestGCSVersion_PrefixHonoursTheObjectCountCap(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	orig := maxGCSObjects
	maxGCSObjects = 2
	t.Cleanup(func() { maxGCSObjects = orig })
	for i := range 3 {
		fake.put(fmt.Sprintf("kb/wiki/p%d.md", i), []byte("---\nid: p\n---\nx\n"))
	}
	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/", Layout: defaultLayout()}
	_, err := GCSVersion(context.Background(), src)
	if err == nil {
		t.Fatal("expected the probe to refuse an over-cap prefix rather than fingerprinting a whole bucket")
	}
	if !strings.Contains(err.Error(), "narrow the prefix") {
		t.Errorf("error = %v, want it to suggest narrowing the prefix", err)
	}
}

// captureStderr collects what fn writes to os.Stderr. The skip-warning
// path writes there directly (see keepFetchableObjects), so this is the
// only way to assert that the reconciliation probe stays quiet.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func TestGCSVersion_RejectsANonGCSSource(t *testing.T) {
	if _, err := GCSVersion(context.Background(), Source{Type: TypeLocal, Path: "x"}); err == nil {
		t.Fatal("expected an error for a non-gcs source")
	}
}

// --- Refreshable -------------------------------------------------------

func TestRefreshable(t *testing.T) {
	cases := []struct {
		name string
		src  Source
		want bool
	}{
		{"gcs with refresh", Source{Type: TypeGCS, Bucket: "b", Prefix: "p/", Refresh: minuteRefresh()}, true},
		{"gcs without refresh", Source{Type: TypeGCS, Bucket: "b", Prefix: "p/"}, false},
		{"pinned generation", Source{Type: TypeGCS, Bucket: "b", Object: "o", Generation: 7, Refresh: minuteRefresh()}, false},
		{"local with refresh", Source{Type: TypeLocal, Path: "p", Refresh: minuteRefresh()}, false},
		{"url with refresh", Source{Type: TypeURL, URL: "https://x", Refresh: minuteRefresh()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.src.Refreshable(); got != tc.want {
				t.Errorf("Refreshable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- configuration -----------------------------------------------------

func TestConfig_RefreshParsesAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFile)
	write(t, path, `collections:
  - name: handbook
    type: gcs
    bucket: example-kb
    prefix: handbook/live/
    refresh:
      interval: 60s
      jitter: 10s
      failure_policy: serve-last-good
    memory:
      type: gcs
      bucket: example-kb
      prefix: handbook/memory/
      refresh:
        interval: 15s
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	src := cfg.Collections[0].Source
	if src.Refresh == nil {
		t.Fatal("refresh: was not parsed")
	}
	if got := src.Refresh.Every(); got != time.Minute {
		t.Errorf("interval = %s, want 1m", got)
	}
	if got := src.Refresh.Jitter.Duration(); got != 10*time.Second {
		t.Errorf("jitter = %s, want 10s", got)
	}
	if got := src.Refresh.Policy(); got != refresh.PolicyServeLastGood {
		t.Errorf("failure_policy = %q", got)
	}
	if src.Memory == nil || src.Memory.Refresh == nil {
		t.Fatal("memory.refresh: was not parsed")
	}
	if got := src.Memory.Refresh.Every(); got != 15*time.Second {
		t.Errorf("memory refresh interval = %s, want 15s", got)
	}
	if !src.Refreshable() {
		t.Error("a gcs prefix source with a refresh block should be refreshable")
	}
}

// TestConfig_RefreshRejections covers the two refusals that keep a
// deployment's guarantees honest — above all that a PINNED source can
// never be talked into moving.
func TestConfig_RefreshRejections(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "pinned generation with refresh",
			yaml: `collections:
  - name: docs
    type: gcs
    bucket: b
    object: kb.tar.gz
    generation: 42
    refresh: {interval: 60s}
`,
			wantErr: "pins this source to one immutable object generation",
		},
		{
			name: "refresh on a local source",
			yaml: `collections:
  - name: docs
    type: local
    path: ./kb
    refresh: {interval: 60s}
`,
			wantErr: "refresh applies to type: gcs only",
		},
		{
			name: "refresh on a url source",
			yaml: `collections:
  - name: docs
    type: url
    url: https://example.com/kb.tar.gz
    sha256: "` + strings.Repeat("a", 64) + `"
    refresh: {interval: 60s}
`,
			wantErr: "refresh applies to type: gcs only",
		},
		{
			name: "interval below the minimum",
			yaml: `collections:
  - name: docs
    type: gcs
    bucket: b
    prefix: p/
    refresh: {interval: 1s}
`,
			wantErr: "below the 5s minimum",
		},
		{
			name: "unknown failure policy",
			yaml: `collections:
  - name: docs
    type: gcs
    bucket: b
    prefix: p/
    refresh: {interval: 60s, failure_policy: explode}
`,
			wantErr: "failure_policy must be one of",
		},
		{
			name: "memory refresh on a local store",
			yaml: `collections:
  - name: docs
    type: gcs
    bucket: b
    prefix: p/
    memory:
      type: local
      path: /srv/memory
      refresh: {interval: 60s}
`,
			wantErr: "refresh applies to type: gcs only",
		},
		{
			name: "interval without a unit",
			yaml: `collections:
  - name: docs
    type: gcs
    bucket: b
    prefix: p/
    refresh: {interval: 60}
`,
			wantErr: "with a unit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ConfigFile)
			write(t, path, tc.yaml)
			_, err := LoadFile(path)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestResolveRuntimeCollections_CarriesTheVersionToken proves the
// startup resolve hands the reconciliation loop the token it needs to
// recognise "nothing has changed" on its very first tick.
func TestResolveRuntimeCollections_CarriesTheVersionToken(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("bundles/kb.tar.gz", kbTarGz(t, map[string]string{"wiki/index.md": "---\nid: index\n---\n# I\n"}))

	path := filepath.Join(t.TempDir(), ConfigFile)
	write(t, path, "collections:\n  - name: docs\n    type: gcs\n    bucket: kb\n    object: bundles/kb.tar.gz\n    refresh: {interval: 60s}\n")

	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if cols[0].Version != "1" {
		t.Errorf("Version = %q, want the resolved object generation", cols[0].Version)
	}
	probed, err := GCSVersion(context.Background(), cols[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	if probed != cols[0].Version {
		t.Errorf("probe = %q but startup resolved %q — the first tick would re-resolve for nothing", probed, cols[0].Version)
	}
}

// TestResolveRuntimeCollections_LocalCarriesNoVersion: a source with no
// version token must not invent one.
func TestResolveRuntimeCollections_LocalCarriesNoVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFile)
	write(t, path, "content:\n  type: local\n  path: .\n")
	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if cols[0].Version != "" {
		t.Errorf("Version = %q, want empty for a local source", cols[0].Version)
	}
}
