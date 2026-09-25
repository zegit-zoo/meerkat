package contentsource

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: S3's own ETag is an MD5; the fake mirrors it.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/s3api"
)

// s3_test.go drives type: s3 through the real FetchS3 / S3Version code
// path — the shared objectstore.go logic parametrised by s3Kind —
// against a fake objectStore that behaves like a conformant S3: ETags
// are the MD5 of the body and a read with a stale ETag fails the
// If-Match precondition. The real providers are exercised by
// s3_conformance_test.go.

// fakeS3 is an in-memory bucket: key -> etag -> content, plus the live
// etag of each key.
type fakeS3 struct {
	objects   map[string]map[string][]byte
	live      map[string]string
	reads     []string
	attrCalls int
	cfg       s3api.Config
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]map[string][]byte{}, live: map[string]string{}}
}

func s3ETag(content []byte) string {
	sum := md5.Sum(content) //nolint:gosec // G401: mirrors S3's ETag, not a security hash.
	return hex.EncodeToString(sum[:])
}

func (f *fakeS3) put(key string, content []byte) string {
	etag := s3ETag(content)
	if f.objects[key] == nil {
		f.objects[key] = map[string][]byte{}
	}
	f.objects[key][etag] = content
	f.live[key] = etag
	return etag
}

func (f *fakeS3) remove(key string) { delete(f.live, key) }

func (f *fakeS3) Attrs(_ context.Context, _, object string) (storedObject, error) {
	f.attrCalls++
	etag, ok := f.live[object]
	if !ok {
		return storedObject{}, fmt.Errorf("NotFound: key %q does not exist", object)
	}
	return storedObject{Name: object, Version: etag, Size: int64(len(f.objects[object][etag]))}, nil
}

func (f *fakeS3) Objects(_ context.Context, _, prefix string) ([]storedObject, error) {
	var out []storedObject
	for key, etag := range f.live {
		if strings.HasPrefix(key, prefix) {
			out = append(out, storedObject{Name: key, Version: etag, Size: int64(len(f.objects[key][etag]))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeS3) Open(_ context.Context, _, object, version string) (io.ReadCloser, error) {
	f.reads = append(f.reads, object+"@"+version)
	live, ok := f.live[object]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %q", object)
	}
	if live != version {
		// What a real If-Match failure looks like to the caller.
		return nil, fmt.Errorf("PreconditionFailed: %q is at %s, not %s", object, live, version)
	}
	return io.NopCloser(bytes.NewReader(f.objects[object][version])), nil
}

func (f *fakeS3) Close() error { return nil }

// useFakeS3 installs f as the client FetchS3 builds, and isolates the
// content cache under a fresh temp dir, for the duration of the test.
func useFakeS3(t *testing.T, f *fakeS3) {
	t.Helper()
	orig := newS3Client
	newS3Client = func(_ context.Context, cfg s3api.Config) (objectStore, error) { f.cfg = cfg; return f, nil }
	t.Cleanup(func() { newS3Client = orig })

	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(base, "AppData", "Local"))
}

func s3Bundle() Source {
	return Source{Type: TypeS3, Bucket: "b", Object: "kb.tar.gz", Endpoint: "http://127.0.0.1:3900", Region: "garage", PathStyle: true, Layout: defaultLayout()}
}

func s3Prefix() Source {
	return Source{Type: TypeS3, Bucket: "b", Prefix: "kb/live/", Endpoint: "http://127.0.0.1:3900", Region: "garage", PathStyle: true, Layout: defaultLayout()}
}

func TestFetchS3_BundleIsKeyedByETagAndReadWithIfMatch(t *testing.T) {
	f := newFakeS3()
	useFakeS3(t, f)
	etag := f.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "# A\n"}))
	src := s3Bundle()

	dir, version, err := FetchS3(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchS3: %v", err)
	}
	if version != etag {
		t.Errorf("version = %q, want the ETag %q", version, etag)
	}
	if f.cfg != (s3api.Config{Endpoint: "http://127.0.0.1:3900", Region: "garage", PathStyle: true}) {
		t.Errorf("client built with %+v", f.cfg)
	}
	// Derive the expectation from os.UserCacheDir() rather than from
	// XDG_CACHE_HOME: on macOS UserCacheDir ignores XDG and returns
	// $HOME/Library/Caches, so the hardcoded form only passed on Linux.
	base, _ := os.UserCacheDir()
	wantDir := filepath.Join(base, "meerkat", "content", "s3")
	if !strings.HasPrefix(dir, wantDir) || filepath.Base(dir) != etag {
		t.Errorf("cache dir = %q, want under %s and named by the ETag", dir, wantDir)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "wiki", "a.md")); string(got) != "# A\n" {
		t.Errorf("extracted content = %q", got)
	}
	if len(f.reads) != 1 || f.reads[0] != "kb.tar.gz@"+etag {
		t.Errorf("reads = %v, want exactly one If-Match read of the ETag", f.reads)
	}
	if got := S3Provenance(src, version); got != "s3://b/kb.tar.gz@"+etag {
		t.Errorf("provenance = %q", got)
	}

	// Same ETag again: a cache hit, one metadata call, no read.
	reads := len(f.reads)
	dir2, version2, err := FetchS3(context.Background(), src)
	if err != nil || dir2 != dir || version2 != version {
		t.Fatalf("second fetch = %q %q %v", dir2, version2, err)
	}
	if len(f.reads) != reads {
		t.Error("a cache hit re-downloaded the bundle")
	}

	// A new object version: a new cache entry named after it.
	etag2 := f.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "# A2\n"}))
	dir3, version3, err := FetchS3(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if version3 != etag2 || dir3 == dir {
		t.Errorf("after overwrite: version %q dir %q (old %q)", version3, dir3, dir)
	}
}

func TestFetchS3_PinnedETagSkipsMetadataAndFailsWhenGone(t *testing.T) {
	f := newFakeS3()
	useFakeS3(t, f)
	etag := f.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "# A\n"}))
	src := s3Bundle()
	src.ETag = etag

	if _, version, err := FetchS3(context.Background(), src); err != nil || version != etag {
		t.Fatalf("pinned fetch = %q %v", version, err)
	}
	if f.attrCalls != 0 {
		t.Errorf("a pinned ETag made %d metadata calls, want none", f.attrCalls)
	}
	if v, err := S3Version(context.Background(), src); err != nil || v != etag {
		t.Errorf("S3Version pinned = %q %v", v, err)
	}
	if src.Refreshable() {
		t.Error("a pinned source must never be refreshable")
	}

	// Overwritten upstream: the pinned version is no longer current, so
	// If-Match fails rather than serving the replacement.
	f.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "# other\n"}))
	useFakeS3(t, f) // fresh cache: force a real read
	_, _, err := FetchS3(context.Background(), src)
	if err == nil || !strings.Contains(err.Error(), "PreconditionFailed") {
		t.Errorf("pinned fetch of a replaced object: err = %v, want a precondition failure", err)
	}
}

func TestFetchS3_SHA256IsVerifiedBeforeExtraction(t *testing.T) {
	f := newFakeS3()
	useFakeS3(t, f)
	bundle := kbTarGz(t, map[string]string{"wiki/a.md": "# A\n"})
	f.put("kb.tar.gz", bundle)
	src := s3Bundle()
	src.SHA256 = strings.Repeat("0", 64)
	if _, _, err := FetchS3(context.Background(), src); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	sum := sha256.Sum256(bundle)
	src.SHA256 = hex.EncodeToString(sum[:])
	if _, _, err := FetchS3(context.Background(), src); err != nil {
		t.Fatalf("matching sha256 refused: %v", err)
	}
}

func TestFetchS3_PrefixFingerprintTracksKeyETagAndSize(t *testing.T) {
	f := newFakeS3()
	useFakeS3(t, f)
	f.put("kb/live/wiki/a.md", []byte("# A\n"))
	f.put("kb/live/wiki/sub/b.md", []byte("# B\n"))
	f.put("kb/live/", nil) // directory placeholder: skipped
	f.put("kb/live/../escape.md", []byte("nope"))
	f.put("other/wiki/c.md", []byte("# C\n"))
	src := s3Prefix()

	dir, version, err := FetchS3(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchS3: %v", err)
	}
	probe, err := S3Version(context.Background(), src)
	if err != nil || probe != version {
		t.Errorf("S3Version = %q %v, want the fetched version %q", probe, err, version)
	}
	for _, want := range []string{"wiki/a.md", "wiki/sub/b.md"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s missing from the tree: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "wiki", "c.md")); err == nil {
		t.Error("an object outside the prefix was mounted")
	}
	if got := S3Provenance(src, version); got != "s3://b/kb/live/*@"+version {
		t.Errorf("provenance = %q", got)
	}

	// Overwrite with different content: new ETag, new fingerprint.
	f.put("kb/live/wiki/a.md", []byte("# A changed\n"))
	v2, _ := S3Version(context.Background(), src)
	if v2 == version {
		t.Error("an overwrite did not move the fingerprint")
	}
	// Delete: new fingerprint.
	f.remove("kb/live/wiki/sub/b.md")
	v3, _ := S3Version(context.Background(), src)
	if v3 == v2 {
		t.Error("a delete did not move the fingerprint")
	}
	// Fetch after changes reads only the current ETags.
	useFakeS3(t, f)
	f.reads = nil
	if _, v4, err := FetchS3(context.Background(), src); err != nil || v4 != v3 {
		t.Fatalf("fetch after changes = %q %v, want %q", v4, err, v3)
	}
	for _, r := range f.reads {
		key, etag, _ := strings.Cut(r, "@")
		if f.live[key] != etag {
			t.Errorf("read %s is not the live ETag", r)
		}
	}
}

func TestListingFingerprint_GCSFormulaIsUnchangedByTheRefactor(t *testing.T) {
	// The pre-refactor GCS code hashed "%s\x00%d\n" per (name, generation).
	// An upgrade must not invalidate every GCS prefix cache or report a
	// spurious change to hot reload, so the size-less form must still
	// produce those exact bytes.
	objs := []storedObject{{Name: "a", Version: "17", Size: 3}, {Name: "b", Version: "42", Size: 9}}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\n", "a", 17)
	fmt.Fprintf(h, "%s\x00%d\n", "b", 42)
	want := hex.EncodeToString(h.Sum(nil))[:32]
	if got := listingFingerprint(objs, false); got != want {
		t.Errorf("GCS fingerprint changed: %s, want %s", got, want)
	}
	if got := listingFingerprint(objs, true); got == want {
		t.Error("the size-aware fingerprint must differ from the size-less one")
	}
}

func TestSafeVersionSegment(t *testing.T) {
	for _, plain := range []string{"1748112233445566", "d41d8cd98f00b204e9800998ecf8427e", "abc-12", "v1.2_3"} {
		if got := safeVersionSegment(plain); got != plain {
			t.Errorf("%q was rewritten to %q", plain, got)
		}
	}
	for _, odd := range []string{"", ".", "..", "a/b", "a b", `"quoted"`} {
		got := safeVersionSegment(odd)
		if got == odd || strings.ContainsAny(got, `/\ "`) || len(got) != 32 {
			t.Errorf("%q -> %q, want a 32-hex hash", odd, got)
		}
	}
}

func TestS3Version_DispatchAndTypeGuards(t *testing.T) {
	f := newFakeS3()
	useFakeS3(t, f)
	f.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "# A\n"}))

	if _, err := ObjectVersion(context.Background(), Source{Type: TypeLocal, Path: "x"}); err == nil {
		t.Error("ObjectVersion on a non-object-store source must fail")
	}
	if _, _, err := FetchObject(context.Background(), Source{Type: TypeURL}); err == nil {
		t.Error("FetchObject on a non-object-store source must fail")
	}
	if _, err := S3Version(context.Background(), Source{Type: TypeGCS, Bucket: "b", Object: "o"}); err == nil {
		t.Error("S3Version on a gcs source must fail")
	}
	if _, _, err := FetchS3(context.Background(), Source{Type: TypeGCS, Bucket: "b", Object: "o"}); err == nil {
		t.Error("FetchS3 on a gcs source must fail")
	}
	v, err := ObjectVersion(context.Background(), s3Bundle())
	if err != nil || v == "" {
		t.Errorf("ObjectVersion(s3) = %q %v", v, err)
	}
	if got := ObjectProvenance(Source{Type: TypeLocal, Bucket: "x"}, "v"); got != "local:x" {
		t.Errorf("ObjectProvenance on a non-store type = %q", got)
	}
}

func TestValidateS3(t *testing.T) {
	ok := s3Prefix()
	cases := []struct {
		name string
		mut  func(*Source)
		want string
	}{
		{"ok prefix", func(*Source) {}, ""},
		{"ok bundle on AWS", func(s *Source) {
			s.Prefix = ""
			s.Object = "kb.tar.gz"
			s.Endpoint = ""
			s.Region = ""
			s.PathStyle = false
		}, ""},
		{"no bucket", func(s *Source) { s.Bucket = "" }, "bucket is required"},
		{"url bucket", func(s *Source) { s.Bucket = "s3://b/x" }, "must be a bucket name"},
		{"neither mode", func(s *Source) { s.Prefix = "" }, "needs either object:"},
		{"both modes", func(s *Source) { s.Object = "o" }, "not both"},
		{"bare endpoint", func(s *Source) { s.Endpoint = "s3.example.net" }, "endpoint must be an http(s):// URL"},
		{"etag on prefix", func(s *Source) { s.ETag = "abc" }, "etag pins one bundle object"},
		{"etag with whitespace", func(s *Source) { s.Prefix = ""; s.Object = "o"; s.ETag = "a b" }, "bare ETag"},
		{"generation on s3", func(s *Source) { s.Generation = 7 }, "generation is a GCS object generation"},
		{"sha256 on prefix", func(s *Source) { s.SHA256 = strings.Repeat("a", 64) }, "sha256 applies to object:"},
		{"bad sha256", func(s *Source) { s.Prefix = ""; s.Object = "o"; s.SHA256 = "zz" }, "64 hex characters"},
	}
	for _, c := range cases {
		s := ok
		c.mut(&s)
		err := s.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestS3_RefreshRules(t *testing.T) {
	s := s3Prefix()
	s.Refresh = minuteRefresh()
	if err := s.Validate(); err != nil {
		t.Errorf("refresh on an s3 prefix: %v", err)
	}
	if !s.Refreshable() {
		t.Error("an s3 source with refresh: must be refreshable")
	}
	b := s3Bundle()
	b.Refresh = minuteRefresh()
	b.ETag = "abc"
	err := b.Validate()
	if err == nil || !strings.Contains(err.Error(), "etag:") {
		t.Errorf("refresh + etag pin: err = %v, want a refusal naming etag:", err)
	}
	if b.Refreshable() {
		t.Error("a pinned s3 bundle must not be refreshable")
	}
	if (Source{Type: TypeS3}).IsObjectStore() != true || (Source{Type: TypeURL}).IsObjectStore() {
		t.Error("IsObjectStore must be true for s3 and false for url")
	}
}

func TestParseConfig_S3Source(t *testing.T) {
	body := `
collections:
  - name: garage
    type: s3
    bucket: kb
    prefix: live/
    endpoint: https://s3.example.net
    region: garage
    path_style: true
    refresh:
      interval: 30s
    memory:
      type: s3
      bucket: kb
      prefix: memory/
      endpoint: https://s3.example.net
      region: garage
      path_style: true
  - name: aws
    type: s3
    bucket: kb
    object: bundles/kb.tar.gz
    etag: "d41d8cd98f00b204e9800998ecf8427e"
`
	cfg, err := parseConfig([]byte(body), "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := cfg.Collections[0]
	if g.Type != TypeS3 || !g.PathStyle || g.Region != "garage" || g.Endpoint != "https://s3.example.net" || !g.Refreshable() {
		t.Errorf("garage collection = %+v", g.Source)
	}
	if g.Memory == nil || g.Memory.Type != "s3" || !g.Memory.PathStyle {
		t.Errorf("garage memory = %+v", g.Memory)
	}
	a := cfg.Collections[1]
	if a.ETag != "d41d8cd98f00b204e9800998ecf8427e" || a.Refreshable() {
		t.Errorf("aws collection = %+v", a.Source)
	}
	if w, desc := a.backendKind(); w || !strings.Contains(desc, "pinned by ETag") {
		t.Errorf("bundle backendKind = %v %q", w, desc)
	}
	if w, _ := g.backendKind(); !w {
		t.Error("an s3 prefix must count as writable")
	}
	if _, err := parseConfig([]byte(strings.Replace(body, "type: s3\n    bucket: kb\n    prefix: live/", "type: s3\n    bucket: kb", 1)), "t.yaml"); err == nil {
		t.Error("an s3 source with neither object nor prefix must be refused")
	}
}

func TestFetchS3_ClientErrorIsSurfaced(t *testing.T) {
	orig := newS3Client
	newS3Client = func(context.Context, s3api.Config) (objectStore, error) { return nil, errors.New("no credentials") }
	t.Cleanup(func() { newS3Client = orig })
	if _, _, err := FetchS3(context.Background(), s3Bundle()); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("err = %v", err)
	}
	if _, err := S3Version(context.Background(), s3Bundle()); err == nil {
		t.Error("S3Version must surface a client construction error")
	}
}
