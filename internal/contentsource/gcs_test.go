package contentsource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
)

// gcs_test.go drives type: gcs through the real FetchGCS code path —
// cache keying, conditional reads, extraction, tree layout, size caps —
// against a fake gcsAPI. Nothing here needs credentials, a bucket, or
// the network: newGCSClient is a package var precisely so the seam can
// be swapped for this fake (see its doc comment).

// fakeGCS is an in-memory bucket: object name -> generation -> content.
type fakeGCS struct {
	objects map[string]map[int64][]byte
	// live is the current generation of each object.
	live map[string]int64
	// reads records every (object, generation) actually fetched, so a
	// test can assert the cache did (or didn't) avoid a download and
	// that the fetch asked for the exact generation it cached under.
	reads []string
	// attrCalls counts metadata lookups, to prove a pinned generation
	// skips them entirely.
	attrCalls int
	closed    bool
}

func newFakeGCS() *fakeGCS {
	return &fakeGCS{objects: map[string]map[int64][]byte{}, live: map[string]int64{}}
}

// put stores content as a new generation of name and returns it.
func (f *fakeGCS) put(name string, content []byte) int64 {
	gen := f.live[name] + 1
	if f.objects[name] == nil {
		f.objects[name] = map[int64][]byte{}
	}
	f.objects[name][gen] = content
	f.live[name] = gen
	return gen
}

// remove deletes an object's live generation, the way a delete under a
// prefix looks to a reader: the name disappears from the listing while
// the historical generations stay addressable.
func (f *fakeGCS) remove(name string) {
	delete(f.live, name)
}

func (f *fakeGCS) Attrs(_ context.Context, _, object string) (gcsObject, error) {
	f.attrCalls++
	gen, ok := f.live[object]
	if !ok {
		return gcsObject{}, fmt.Errorf("object %q does not exist", object)
	}
	return gcsObject{Name: object, Generation: gen, Size: int64(len(f.objects[object][gen]))}, nil
}

func (f *fakeGCS) Objects(_ context.Context, _, prefix string) ([]gcsObject, error) {
	var out []gcsObject
	for name, gen := range f.live {
		if strings.HasPrefix(name, prefix) {
			out = append(out, gcsObject{Name: name, Generation: gen, Size: int64(len(f.objects[name][gen]))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeGCS) Open(_ context.Context, _, object string, generation int64) (io.ReadCloser, error) {
	f.reads = append(f.reads, fmt.Sprintf("%s@%d", object, generation))
	byGen, ok := f.objects[object]
	if !ok {
		return nil, fmt.Errorf("object %q does not exist", object)
	}
	content, ok := byGen[generation]
	if !ok {
		// What a real ifGenerationMatch failure looks like to the caller.
		return nil, fmt.Errorf("generation %d of %q does not exist (precondition failed)", generation, object)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (f *fakeGCS) Close() error { f.closed = true; return nil }

// useFakeGCS installs f as the client FetchGCS builds, and isolates the
// content cache under a fresh temp dir, for the duration of the test.
func useFakeGCS(t *testing.T, f *fakeGCS) {
	t.Helper()
	orig := newGCSClient
	newGCSClient = func(context.Context) (gcsAPI, error) { return f, nil }
	t.Cleanup(func() { newGCSClient = orig })

	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(base, "AppData", "Local"))
}

// kbTarGz builds a content-repo-layout tar.gz with the given files.
func kbTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// --- object (bundle) mode ---

func TestFetchGCS_Object_FetchesExtractsAndCachesByGeneration(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	gen := fake.put("bundles/kb.tar.gz", kbTarGz(t, map[string]string{
		"wiki/index.md": "---\nid: index\n---\n# Index\n",
	}))

	src := Source{Type: TypeGCS, Bucket: "kb-bucket", Object: "bundles/kb.tar.gz", Layout: defaultLayout()}
	dir, version, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS: %v", err)
	}
	if version != "1" {
		t.Errorf("version = %q, want the object generation %q", version, "1")
	}
	if got := readFile(t, dir, "wiki/index.md"); !strings.Contains(got, "# Index") {
		t.Errorf("extracted content = %q", got)
	}
	// The read must have asked for exactly the generation the cache
	// entry is named after — that's the immutable-retrieval guarantee.
	if len(fake.reads) != 1 || fake.reads[0] != fmt.Sprintf("bundles/kb.tar.gz@%d", gen) {
		t.Errorf("reads = %v, want exactly one read of generation %d", fake.reads, gen)
	}
	if !strings.HasSuffix(filepath.ToSlash(dir), "/1") {
		t.Errorf("cache dir %q should end in the generation", dir)
	}

	// Second fetch of the same generation: metadata only, no download.
	dir2, version2, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("second FetchGCS: %v", err)
	}
	if dir2 != dir || version2 != version {
		t.Errorf("second fetch = (%q, %q), want the cached (%q, %q)", dir2, version2, dir, version)
	}
	if len(fake.reads) != 1 {
		t.Errorf("reads = %v, want the cache hit to have downloaded nothing more", fake.reads)
	}
}

// TestFetchGCS_Object_NewGenerationInvalidatesCache is the cache-
// invalidation contract: overwriting the object bumps its generation,
// which is the cache key, so the next fetch serves the new content.
func TestFetchGCS_Object_NewGenerationInvalidatesCache(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "---\nid: a\n---\nold\n"}))

	src := Source{Type: TypeGCS, Bucket: "b", Object: "kb.tar.gz", Layout: defaultLayout()}
	dirOld, verOld, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS: %v", err)
	}
	if !strings.Contains(readFile(t, dirOld, "wiki/a.md"), "old") {
		t.Fatal("first fetch did not serve the first generation")
	}

	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "---\nid: a\n---\nnew\n"}))
	dirNew, verNew, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS after overwrite: %v", err)
	}
	if verNew == verOld {
		t.Errorf("version did not change after a new generation (%q)", verNew)
	}
	if dirNew == dirOld {
		t.Errorf("cache dir did not change after a new generation (%q)", dirNew)
	}
	if !strings.Contains(readFile(t, dirNew, "wiki/a.md"), "new") {
		t.Error("second fetch served stale content")
	}
	// The old entry is still there and untouched — generations are
	// immutable, so a rollback re-uses it rather than re-downloading.
	if !strings.Contains(readFile(t, dirOld, "wiki/a.md"), "old") {
		t.Error("the previous generation's cache entry was clobbered")
	}
}

// TestFetchGCS_Object_PinnedGenerationSkipsMetadataLookup proves an
// explicit generation: is a real pin — the current generation is never
// consulted, so a later overwrite cannot change what is served.
func TestFetchGCS_Object_PinnedGenerationSkipsMetadataLookup(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	pinned := fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "---\nid: a\n---\npinned\n"}))
	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "---\nid: a\n---\nlatest\n"}))

	src := Source{Type: TypeGCS, Bucket: "b", Object: "kb.tar.gz", Generation: pinned, Layout: defaultLayout()}
	dir, version, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS: %v", err)
	}
	if fake.attrCalls != 0 {
		t.Errorf("attrCalls = %d, want 0 (a pinned generation needs no metadata lookup)", fake.attrCalls)
	}
	if version != fmt.Sprintf("%d", pinned) {
		t.Errorf("version = %q, want the pinned generation %d", version, pinned)
	}
	if !strings.Contains(readFile(t, dir, "wiki/a.md"), "pinned") {
		t.Error("a pinned generation served the newer object")
	}
}

// TestFetchGCS_Object_MissingGenerationFails covers the conditional
// read failing (a real ifGenerationMatch precondition failure): nothing
// is cached and the error names the object.
func TestFetchGCS_Object_MissingGenerationFails(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "x"}))

	src := Source{Type: TypeGCS, Bucket: "b", Object: "kb.tar.gz", Generation: 999, Layout: defaultLayout()}
	_, _, err := FetchGCS(context.Background(), src)
	if err == nil {
		t.Fatal("expected an error for a generation that doesn't exist")
	}
	if !strings.Contains(err.Error(), "kb.tar.gz") {
		t.Errorf("error = %v, want it to name the object", err)
	}
}

// TestFetchGCS_Object_SHA256MismatchRefusesToCache: sha256 is optional
// for gcs (generation already pins the bytes), but when supplied it is
// verified before anything is extracted.
func TestFetchGCS_Object_SHA256MismatchRefusesToCache(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "x"}))

	src := Source{
		Type: TypeGCS, Bucket: "b", Object: "kb.tar.gz",
		SHA256: strings.Repeat("0", 64), Layout: defaultLayout(),
	}
	_, _, err := FetchGCS(context.Background(), src)
	if err == nil {
		t.Fatal("expected a sha256 mismatch error")
	}
	if !strings.Contains(err.Error(), "refusing to extract or cache") {
		t.Errorf("error = %v, want it to say nothing was extracted or cached", err)
	}
}

func TestFetchGCS_Object_SHA256MatchAccepted(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	body := kbTarGz(t, map[string]string{"wiki/a.md": "---\nid: a\n---\nbody\n"})
	fake.put("kb.tar.gz", body)
	sum := sha256.Sum256(body)

	src := Source{
		Type: TypeGCS, Bucket: "b", Object: "kb.tar.gz",
		SHA256: hex.EncodeToString(sum[:]), Layout: defaultLayout(),
	}
	dir, _, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS with a matching sha256: %v", err)
	}
	if !strings.Contains(readFile(t, dir, "wiki/a.md"), "body") {
		t.Error("content not served after a matching digest")
	}
}

// --- prefix (directory tree) mode ---

func TestFetchGCS_Prefix_MirrorsTreeAndCachesByListingFingerprint(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb/live/wiki/index.md", []byte("---\nid: index\n---\n# Index\n"))
	fake.put("kb/live/wiki/concepts/widgets.md", []byte("---\nid: concepts/widgets\n---\n# Widgets\n"))
	fake.put("kb/live/ingestion/sources.yaml", []byte("sources: []\n"))
	// Outside the prefix — must not be mounted.
	fake.put("other/secret.md", []byte("nope"))

	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/live/", Layout: defaultLayout()}
	dir, version, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS: %v", err)
	}
	if version == "" {
		t.Error("prefix mode must produce a listing fingerprint")
	}
	if got := readFile(t, dir, "wiki/concepts/widgets.md"); !strings.Contains(got, "# Widgets") {
		t.Errorf("nested object not mirrored: %q", got)
	}
	if readFile(t, dir, "ingestion/sources.yaml") != "sources: []\n" {
		t.Error("non-markdown artifact not mirrored")
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.md")); err == nil {
		t.Error("an object outside the prefix was mounted")
	}
	downloads := len(fake.reads)
	if downloads != 3 {
		t.Errorf("downloaded %d objects (%v), want the 3 under the prefix", downloads, fake.reads)
	}

	// Unchanged listing -> same fingerprint -> cache hit, no downloads.
	dir2, version2, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("second FetchGCS: %v", err)
	}
	if dir2 != dir || version2 != version {
		t.Errorf("second fetch = (%q,%q), want the cached (%q,%q)", dir2, version2, dir, version)
	}
	if len(fake.reads) != downloads {
		t.Errorf("cache hit still downloaded: %v", fake.reads)
	}
}

// TestFetchGCS_Prefix_AnyObjectChangeInvalidates covers all three ways a
// prefix's content can change — overwrite, add, and (implicitly, via the
// fingerprint) delete.
func TestFetchGCS_Prefix_AnyObjectChangeInvalidates(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb/wiki/a.md", []byte("---\nid: a\n---\nv1\n"))
	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/", Layout: defaultLayout()}

	_, v1, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}

	fake.put("kb/wiki/a.md", []byte("---\nid: a\n---\nv2\n")) // overwrite
	dir2, v2, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if v2 == v1 {
		t.Error("overwriting an object did not change the fingerprint")
	}
	if !strings.Contains(readFile(t, dir2, "wiki/a.md"), "v2") {
		t.Error("stale content served after an overwrite")
	}

	fake.put("kb/wiki/b.md", []byte("---\nid: b\n---\nnew\n")) // add
	dir3, v3, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if v3 == v2 {
		t.Error("adding an object did not change the fingerprint")
	}
	if !strings.Contains(readFile(t, dir3, "wiki/b.md"), "new") {
		t.Error("added object not served")
	}
}

// TestFetchGCS_Prefix_SkipsUnsafeAndPlaceholderObjects proves an object
// name that would escape the mount (or a zero-byte "directory") is
// skipped rather than written, and doesn't take the collection down.
func TestFetchGCS_Prefix_SkipsUnsafeAndPlaceholderObjects(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("kb/wiki/ok.md", []byte("---\nid: ok\n---\nfine\n"))
	fake.put("kb/wiki/", []byte("")) // directory placeholder
	fake.put("kb/../escape.md", []byte("should never be written"))

	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/", Layout: defaultLayout()}
	dir, _, err := FetchGCS(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchGCS: %v", err)
	}
	if !strings.Contains(readFile(t, dir, "wiki/ok.md"), "fine") {
		t.Error("the safe object was not mounted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.md")); err == nil {
		t.Error("an object with a traversing name escaped the mount")
	}
}

// TestFetchGCS_Prefix_ObjectCountCapRefuses proves a mistyped (too
// broad) prefix fails loudly instead of downloading a whole bucket.
func TestFetchGCS_Prefix_ObjectCountCapRefuses(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	orig := maxGCSObjects
	maxGCSObjects = 2
	t.Cleanup(func() { maxGCSObjects = orig })

	for i := range 3 {
		fake.put(fmt.Sprintf("kb/wiki/p%d.md", i), []byte("---\nid: p\n---\nx\n"))
	}

	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/", Layout: defaultLayout()}
	_, _, err := FetchGCS(context.Background(), src)
	if err == nil {
		t.Fatal("expected the object-count cap to refuse")
	}
	if !strings.Contains(err.Error(), "narrow the prefix") {
		t.Errorf("error = %v, want it to suggest narrowing the prefix", err)
	}
}

// TestFetchGCS_Prefix_PerFileCapRefuses proves a single oversized object
// is refused rather than buffered to disk unbounded.
func TestFetchGCS_Prefix_PerFileCapRefuses(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	orig := maxExtractedFileBytes
	maxExtractedFileBytes = 16
	t.Cleanup(func() { maxExtractedFileBytes = orig })

	fake.put("kb/wiki/big.md", bytes.Repeat([]byte("x"), 100))

	src := Source{Type: TypeGCS, Bucket: "b", Prefix: "kb/", Layout: defaultLayout()}
	_, _, err := FetchGCS(context.Background(), src)
	if err == nil {
		t.Fatal("expected the per-file cap to refuse")
	}
	if !strings.Contains(err.Error(), "per-file cap") {
		t.Errorf("error = %v, want it to name the per-file cap", err)
	}
}

// --- validation / provenance ---

func TestValidateGCS(t *testing.T) {
	cases := []struct {
		name    string
		src     Source
		wantErr string
	}{
		{"no bucket", Source{Type: TypeGCS, Object: "o"}, "bucket is required"},
		{"bucket as a path", Source{Type: TypeGCS, Bucket: "b/o", Object: "o"}, "not a path or gs:// URL"},
		{"neither object nor prefix", Source{Type: TypeGCS, Bucket: "b"}, "either object:"},
		{"both object and prefix", Source{Type: TypeGCS, Bucket: "b", Object: "o", Prefix: "p/"}, "not both"},
		{"bad sha256", Source{Type: TypeGCS, Bucket: "b", Object: "o", SHA256: "nope"}, "64 hex characters"},
		{"sha256 with prefix", Source{Type: TypeGCS, Bucket: "b", Prefix: "p/", SHA256: strings.Repeat("a", 64)}, "applies to object:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.src
			src.Layout = defaultLayout()
			err := src.Validate()
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateGCS_Accepted(t *testing.T) {
	for _, src := range []Source{
		{Type: TypeGCS, Bucket: "b", Object: "bundles/kb.tar.gz", Layout: defaultLayout()},
		{Type: TypeGCS, Bucket: "b", Prefix: "kb/live/", Layout: defaultLayout()},
		{Type: TypeGCS, Bucket: "b", Object: "o", Generation: 42, Layout: defaultLayout()},
	} {
		if err := src.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", src, err)
		}
	}
}

func TestGCSProvenance(t *testing.T) {
	obj := GCSProvenance(Source{Bucket: "kb", Object: "bundles/v3.tar.gz"}, "17")
	if obj != "gcs://kb/bundles/v3.tar.gz@17" {
		t.Errorf("object provenance = %q", obj)
	}
	pfx := GCSProvenance(Source{Bucket: "kb", Prefix: "live/"}, "abc123")
	if pfx != "gcs://kb/live/*@abc123" {
		t.Errorf("prefix provenance = %q", pfx)
	}
}

// TestFetchGCS_RejectsNonGCSSource guards the direct-call path: FetchGCS
// re-validates rather than trusting its caller.
func TestFetchGCS_RejectsNonGCSSource(t *testing.T) {
	_, _, err := FetchGCS(context.Background(), Source{Type: TypeLocal, Path: "x"})
	if err == nil {
		t.Fatal("expected an error for a non-gcs source")
	}
}

// TestResolveRuntimeCollections_GCS wires a gcs source through the real
// config -> resolve path, proving the type is reachable from a
// content-source.yaml and reports gcs: provenance.
func TestResolveRuntimeCollections_GCS(t *testing.T) {
	fake := newFakeGCS()
	useFakeGCS(t, fake)
	fake.put("bundles/kb.tar.gz", kbTarGz(t, map[string]string{"wiki/index.md": "---\nid: index\n---\n# I\n"}))

	path := filepath.Join(t.TempDir(), ConfigFile)
	write(t, path, "collections:\n  - name: docs\n    type: gcs\n    bucket: kb-bucket\n    object: bundles/kb.tar.gz\n")

	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 1 || cols[0].Name != "docs" {
		t.Fatalf("got %+v, want one collection named docs", cols)
	}
	if want := "gcs://kb-bucket/bundles/kb.tar.gz@1"; cols[0].Provenance != want {
		t.Errorf("provenance = %q, want %q", cols[0].Provenance, want)
	}
	if !strings.Contains(readFile(t, cols[0].Dir, "wiki/index.md"), "# I") {
		t.Error("resolved directory does not hold the bundle's content")
	}
}

// TestResolveRuntimeCollections_GCSClientErrorSurfaces proves a
// credential/connection failure is reported, not swallowed into the
// embedded fallback.
func TestResolveRuntimeCollections_GCSClientErrorSurfaces(t *testing.T) {
	orig := newGCSClient
	newGCSClient = func(context.Context) (gcsAPI, error) {
		return nil, errors.New("could not find default credentials")
	}
	t.Cleanup(func() { newGCSClient = orig })

	path := filepath.Join(t.TempDir(), ConfigFile)
	write(t, path, "collections:\n  - name: docs\n    type: gcs\n    bucket: b\n    object: o.tar.gz\n")

	_, err := ResolveRuntimeCollections(context.Background(), path)
	if err == nil {
		t.Fatal("expected the client error to surface")
	}
	if !strings.Contains(err.Error(), "default credentials") {
		t.Errorf("error = %v, want the underlying credential error", err)
	}
}
