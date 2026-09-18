package memory

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: S3's own ETag is an MD5; the fake mirrors it.
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/s3api"
)

// s3_test.go drives S3Store through a fake s3MemoryAPI that behaves the
// way a conformant S3 does: ETags are the MD5 of the body, If-None-Match:
// * refuses an existing key, If-Match refuses a changed one. The real
// backend's behaviour is checked by s3_conformance_test.go.

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]fakeS3Object
	writes  int
	// enforce controls whether conditional writes are honoured; a test
	// flips it to model a provider that ignores preconditions.
	enforce bool
}

type fakeS3Object struct {
	body []byte
	etag string
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string]fakeS3Object{}, enforce: true} }

func fakeETag(body []byte) string {
	sum := md5.Sum(body) //nolint:gosec // G401: mirrors S3's ETag, not a security hash.
	return hex.EncodeToString(sum[:])
}

func (f *fakeS3) List(_ context.Context, _, prefix string) ([]s3MemoryObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []s3MemoryObject
	for k, o := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3MemoryObject{Key: k, ETag: o.etag})
		}
	}
	return out, nil
}

func (f *fakeS3) Read(_ context.Context, _, key, ifMatch string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return nil, "", errS3NotExist
	}
	if ifMatch != "" && ifMatch != o.etag {
		return nil, "", errS3Precondition
	}
	return o.body, o.etag, nil
}

func (f *fakeS3) Head(_ context.Context, _, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return "", errS3NotExist
	}
	return o.etag, nil
}

func (f *fakeS3) Write(_ context.Context, _, key string, body []byte, createOnly bool, ifMatch, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, exists := f.objects[key]
	if f.enforce {
		switch {
		case createOnly && exists:
			return "", fmt.Errorf("%w: object exists", errS3Precondition)
		case !createOnly && !exists:
			return "", fmt.Errorf("%w: object is gone", errS3Precondition)
		case !createOnly && ifMatch != o.etag:
			return "", fmt.Errorf("%w: etag is %s, not %s", errS3Precondition, o.etag, ifMatch)
		}
	}
	etag := fakeETag(body)
	f.objects[key] = fakeS3Object{body: append([]byte(nil), body...), etag: etag}
	f.writes++
	return etag, nil
}

func (f *fakeS3) Delete(_ context.Context, _, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeS3) WriteUnconditional(_ context.Context, _, key string, body []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeS3Object{body: append([]byte(nil), body...), etag: fakeETag(body)}
	f.writes++
	return nil
}

func newFakeS3Store(t *testing.T) (*S3Store, *fakeS3) {
	t.Helper()
	api := newFakeS3()
	return newS3StoreWithAPI(api, "bucket", "kb/memory/", ""), api
}

func TestS3_ConditionalWritePreconditions(t *testing.T) {
	ctx := context.Background()
	s, api := newFakeS3Store(t)

	v1, err := s.Put(ctx, "team/note.md", []byte("one"), CreateOnly())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v1 != Version(fakeETag([]byte("one"))) {
		t.Errorf("version = %q, want the object's ETag", v1)
	}

	// If-None-Match: * on a key that now exists.
	before := api.writes
	_, err = s.Put(ctx, "team/note.md", []byte("clobber"), CreateOnly())
	assertConflict(t, err, v1)
	if api.writes != before {
		t.Error("a refused create still stored bytes")
	}

	v2, err := s.Put(ctx, "team/note.md", []byte("two"), UpdateFrom(v1))
	if err != nil {
		t.Fatalf("update from current: %v", err)
	}
	if v2 == v1 {
		t.Error("an update must issue a new version")
	}

	// If-Match against a version that is no longer current.
	before = api.writes
	_, err = s.Put(ctx, "team/note.md", []byte("three"), UpdateFrom(v1))
	assertConflict(t, err, v2)
	if api.writes != before {
		t.Error("a refused update still stored bytes")
	}

	// Updating something that was deleted underneath us.
	delete(api.objects, "kb/memory/team/note.md")
	_, err = s.Put(ctx, "team/note.md", []byte("four"), UpdateFrom(v2))
	if !errors.Is(err, ErrConflict) {
		t.Errorf("update of a vanished object: err = %v, want ErrConflict", err)
	}
}

func TestS3_VersionsAreOpaqueButMustBeOnes(t *testing.T) {
	ctx := context.Background()
	s, _ := newFakeS3Store(t)
	for _, bad := range []Version{"", "has space", "a\nb"} {
		if _, err := s.Put(ctx, "team/x.md", []byte("x"), UpdateFrom(bad)); err == nil || errors.Is(err, ErrConflict) {
			t.Errorf("UpdateFrom(%q): err = %v, want a validation error, not a conflict", bad, err)
		}
	}
	// A quoted ETag (as a client might echo it) is accepted as its bare form.
	v, err := s.Put(ctx, "team/x.md", []byte("x"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "team/x.md", []byte("y"), UpdateFrom(`"`+v+`"`)); err != nil {
		t.Errorf("quoted version refused: %v", err)
	}
}

func TestS3_StatLoadAndFingerprint(t *testing.T) {
	ctx := context.Background()
	s, api := newFakeS3Store(t)

	if _, ok, err := s.Stat(ctx, "team/missing.md"); err != nil || ok {
		t.Errorf("Stat(missing) = ok=%v err=%v, want absent", ok, err)
	}
	fp0, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}

	v, err := s.Put(ctx, "team/a.md", []byte("alpha"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Stat(ctx, "team/a.md")
	if err != nil || !ok || got != v {
		t.Errorf("Stat = %q ok=%v err=%v, want %q", got, ok, err, v)
	}
	fp1, _ := s.Fingerprint(ctx)
	if fp1 == fp0 {
		t.Error("a write must move the fingerprint")
	}

	// Staged and non-document objects are neither loaded nor fingerprinted.
	if _, err := s.Stage(ctx, StagingPrefix+"/team/ns/b.md", []byte("pending")); err != nil {
		t.Fatal(err)
	}
	if err := api.WriteUnconditional(ctx, "bucket", "kb/memory/team/notes.txt", []byte("not markdown"), ""); err != nil {
		t.Fatal(err)
	}
	fp2, _ := s.Fingerprint(ctx)
	if fp2 != fp1 {
		t.Error("staged / non-.md objects must not move the fingerprint")
	}
	recs, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Key != "team/a.md" || string(recs[0].Body) != "alpha" || recs[0].Version != v {
		t.Errorf("Load = %+v, want exactly team/a.md@%s", recs, v)
	}

	// A document overwritten between listing and read is skipped, not
	// loaded under a stale version.
	api.mu.Lock()
	api.objects["kb/memory/team/a.md"] = fakeS3Object{body: []byte("changed"), etag: "other"}
	api.mu.Unlock()
	// Fingerprint reflects the new etag; Load's per-object If-Match is
	// what the listing reported, which is now current again, so it loads.
	recs, _ = s.Load(ctx)
	if len(recs) != 1 || string(recs[0].Body) != "changed" {
		t.Errorf("Load after overwrite = %+v", recs)
	}
}

func TestS3_StageOnlyUnderTheStagingPrefix(t *testing.T) {
	ctx := context.Background()
	s, _ := newFakeS3Store(t)
	if _, err := s.Stage(ctx, "team/live.md", []byte("x")); err == nil {
		t.Error("Stage outside the staging prefix must be refused")
	}
	loc, err := s.Stage(ctx, StagingPrefix+"/team/ns/x.md", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if loc != "s3://bucket/kb/memory/"+StagingPrefix+"/team/ns/x.md" {
		t.Errorf("Stage location = %q", loc)
	}
}

func TestS3_DescribeLocationAndBackend(t *testing.T) {
	s, _ := newFakeS3Store(t)
	if s.Describe() != "s3://bucket/kb/memory/" {
		t.Errorf("Describe = %q", s.Describe())
	}
	if s.Location("team/x.md") != "s3://bucket/kb/memory/team/x.md" {
		t.Errorf("Location = %q", s.Location("team/x.md"))
	}
	if Backend(s) != "s3" {
		t.Errorf("Backend = %q", Backend(s))
	}
	var _ Fingerprinter = s
}

func TestS3_ProbeRefusesAProviderThatIgnoresPreconditions(t *testing.T) {
	// Without the probe, a provider that ignores preconditions makes the
	// store accept a stale write silently — the exact lost update the
	// backend exists to rule out. The probe turns that into a refusal
	// to open.
	ctx := context.Background()
	s, api := newFakeS3Store(t)
	api.enforce = false
	v1, _ := s.Put(ctx, "team/n.md", []byte("one"), CreateOnly())
	if _, err := s.Put(ctx, "team/n.md", []byte("two"), UpdateFrom(v1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "team/n.md", []byte("stale"), UpdateFrom(v1)); err != nil {
		t.Fatalf("the non-enforcing fake must accept the stale write silently (that is the hazard), got %v", err)
	}

	err := s.verifyConditionalWrites(ctx)
	if !errors.Is(err, ErrConditionalWritesNotEnforced) {
		t.Fatalf("probe against a non-enforcing backend: err = %v, want ErrConditionalWritesNotEnforced", err)
	}
	if !strings.Contains(err.Error(), "single_writer") {
		t.Errorf("the refusal must name the escape hatch: %v", err)
	}
	api.enforce = true
	if err := s.verifyConditionalWrites(ctx); err != nil {
		t.Errorf("probe against an enforcing backend: %v", err)
	}
	// The probe cleans up after itself and never touches live documents.
	for k := range api.objects {
		if strings.Contains(k, "_probe") {
			t.Errorf("probe object %s left behind", k)
		}
	}
	recs, _ := s.Load(ctx)
	if len(recs) != 1 || recs[0].Key != "team/n.md" {
		t.Errorf("Load after probes = %+v", recs)
	}
}

func TestS3_SingleWriterEnforcesInProcess(t *testing.T) {
	ctx := context.Background()
	s, api := newFakeS3Store(t)
	api.enforce = false // a Garage-like backend
	s.singleWriter = true

	v1, err := s.Put(ctx, "team/n.md", []byte("one"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}
	before := api.writes
	_, err = s.Put(ctx, "team/n.md", []byte("clobber"), CreateOnly())
	assertConflict(t, err, v1)
	if api.writes != before {
		t.Error("a refused create still stored bytes")
	}
	v2, err := s.Put(ctx, "team/n.md", []byte("two"), UpdateFrom(v1))
	if err != nil {
		t.Fatal(err)
	}
	before = api.writes
	_, err = s.Put(ctx, "team/n.md", []byte("stale"), UpdateFrom(v1))
	assertConflict(t, err, v2)
	if api.writes != before {
		t.Error("a refused stale update still stored bytes")
	}
	delete(api.objects, "kb/memory/team/n.md")
	if _, err := s.Put(ctx, "team/n.md", []byte("x"), UpdateFrom(v2)); !errors.Is(err, ErrConflict) {
		t.Errorf("update of a vanished object = %v, want ErrConflict", err)
	}
}

func TestOpenS3_ValidatesAndUsesTheDefaultChainClient(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenS3(ctx, nil); err == nil {
		t.Error("nil spec must be refused")
	}
	if _, err := OpenS3(ctx, &Spec{Type: BackendS3}); err == nil {
		t.Error("empty bucket must be refused")
	}

	orig := newS3MemoryClient
	t.Cleanup(func() { newS3MemoryClient = orig })
	var seen s3api.Config
	newS3MemoryClient = func(_ context.Context, cfg s3api.Config) (s3MemoryAPI, error) {
		seen = cfg
		return newFakeS3(), nil
	}
	spec := &Spec{Type: BackendS3, Bucket: "b", Prefix: "kb/memory", Endpoint: "http://127.0.0.1:3900", Region: "garage", PathStyle: true, SSE: "AES256"}
	st, err := spec.Open(ctx, "")
	if err != nil {
		t.Fatalf("Spec.Open: %v", err)
	}
	s, ok := st.(*S3Store)
	if !ok {
		t.Fatalf("Spec.Open returned %T, want *S3Store", st)
	}
	if seen != (s3api.Config{Endpoint: "http://127.0.0.1:3900", Region: "garage", PathStyle: true}) {
		t.Errorf("client config = %+v", seen)
	}
	if s.prefix != "kb/memory/" || s.sse != "AES256" || s.singleWriter {
		t.Errorf("store = prefix %q sse %q singleWriter %v", s.prefix, s.sse, s.singleWriter)
	}

	// A backend that ignores preconditions is refused at open — unless
	// the operator asserts single_writer.
	lax := newFakeS3()
	lax.enforce = false
	newS3MemoryClient = func(context.Context, s3api.Config) (s3MemoryAPI, error) { return lax, nil }
	if _, err := OpenS3(ctx, spec); !errors.Is(err, ErrConditionalWritesNotEnforced) {
		t.Errorf("OpenS3 on a non-enforcing backend: err = %v", err)
	}
	single := *spec
	single.SingleWriter = true
	s2, err := OpenS3(ctx, &single)
	if err != nil || !s2.singleWriter {
		t.Errorf("OpenS3 single_writer = %v %v", s2, err)
	}
	if len(lax.objects) != 0 {
		t.Errorf("the refused open left %d probe objects behind", len(lax.objects))
	}
}
