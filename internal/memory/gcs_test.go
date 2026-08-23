package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
)

// gcs_test.go runs the GCS backend against an in-memory fake that
// implements the generation semantics GCS itself does: every write
// bumps a generation, and a write whose ifGenerationMatch does not hold
// is refused. Nothing here needs credentials, a bucket, or the network.

// fakeGCS is a tiny, concurrency-safe object store with generations.
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	nextGen int64
	closed  bool
	// writes counts every accepted write, so a test can assert that a
	// refused precondition really did not store anything.
	writes int
}

type fakeObject struct {
	body []byte
	gen  int64
}

func newFakeGCS() *fakeGCS {
	return &fakeGCS{objects: map[string]fakeObject{}, nextGen: 1000}
}

func (f *fakeGCS) List(_ context.Context, _, prefix string) ([]gcsObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gcsObject
	for name, o := range f.objects {
		if strings.HasPrefix(name, prefix) {
			out = append(out, gcsObject{Name: name, Generation: o.gen})
		}
	}
	return out, nil
}

func (f *fakeGCS) Read(_ context.Context, _, object string, generation int64) ([]byte, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[object]
	if !ok {
		return nil, 0, errFakeNotExist
	}
	if generation > 0 && generation != o.gen {
		return nil, 0, errFakeNotExist
	}
	return o.body, o.gen, nil
}

func (f *fakeGCS) Write(_ context.Context, _, object string, body []byte, ifGeneration int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, exists := f.objects[object]
	switch {
	case ifGeneration == 0 && exists:
		return 0, fmt.Errorf("%w: object exists", errGCSPrecondition)
	case ifGeneration > 0 && !exists:
		return 0, fmt.Errorf("%w: object is gone", errGCSPrecondition)
	case ifGeneration > 0 && ifGeneration != o.gen:
		return 0, fmt.Errorf("%w: generation is %d, not %d", errGCSPrecondition, o.gen, ifGeneration)
	}
	f.nextGen++
	f.objects[object] = fakeObject{body: append([]byte(nil), body...), gen: f.nextGen}
	f.writes++
	return f.nextGen, nil
}

func (f *fakeGCS) WriteUnconditional(_ context.Context, _, object string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextGen++
	f.objects[object] = fakeObject{body: append([]byte(nil), body...), gen: f.nextGen}
	f.writes++
	return nil
}

func (f *fakeGCS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// errFakeNotExist is the real library sentinel, so the store's own
// isNotExist translation is what the tests below exercise rather than a
// test-only shortcut.
var errFakeNotExist = fmt.Errorf("fake gcs: %w", storage.ErrObjectNotExist)

func newFakeGCSStore(t *testing.T) (*GCSStore, *fakeGCS) {
	t.Helper()
	api := newFakeGCS()
	return newGCSStoreWithAPI(api, "bucket", "kb/memory/"), api
}

func TestGCS_GenerationMatchPreconditions(t *testing.T) {
	ctx := context.Background()
	s, api := newFakeGCSStore(t)

	v1, err := s.Put(ctx, "team/note.md", []byte("one"), CreateOnly())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v1 != "1001" {
		t.Errorf("version = %q, want the object generation", v1)
	}

	// if-generation-match: 0 on an object that now exists.
	before := api.writes
	_, err = s.Put(ctx, "team/note.md", []byte("clobber"), CreateOnly())
	assertConflict(t, err, v1)
	if api.writes != before {
		t.Error("a refused create still stored bytes")
	}

	v2, err := s.Put(ctx, "team/note.md", []byte("two"), UpdateFrom(v1))
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// The stale generation is refused, and the conflict reports the one
	// that is actually there, so a retry needs no extra read.
	before = api.writes
	_, err = s.Put(ctx, "team/note.md", []byte("three"), UpdateFrom(v1))
	assertConflict(t, err, v2)
	if api.writes != before {
		t.Error("a refused update still stored bytes")
	}
}

func TestGCS_UpdateOfAMissingObjectIsAConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := newFakeGCSStore(t)
	_, err := s.Put(ctx, "team/gone.md", []byte("x"), UpdateFrom("1234"))
	assertConflict(t, err, "")
}

func TestGCS_RefusesAVersionItNeverIssued(t *testing.T) {
	ctx := context.Background()
	s, _ := newFakeGCSStore(t)
	// A local-store content hash is not an object generation. Passing
	// one must be a plain argument error, not a silent create.
	_, err := s.Put(ctx, "team/note.md", []byte("x"), UpdateFrom("3f2a1c0b9d8e7f60"))
	if err == nil {
		t.Fatal("Put with a foreign version token succeeded")
	}
	if errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want an argument error rather than a conflict", err)
	}
}

func TestGCS_LoadSkipsStagingAndReadsThePinnedGeneration(t *testing.T) {
	ctx := context.Background()
	s, api := newFakeGCSStore(t)

	if _, err := s.Put(ctx, "team/live.md", []byte("live"), CreateOnly()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(ctx, StagingPrefix+"/team/ns/p.md", []byte("pending")); err != nil {
		t.Fatal(err)
	}
	// An object under the prefix that isn't a memory document at all.
	if err := api.WriteUnconditional(ctx, "bucket", "kb/memory/notes.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}

	records, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(records) != 1 || records[0].Key != "team/live.md" {
		t.Fatalf("Load = %+v, want just team/live.md", records)
	}
	if string(records[0].Body) != "live" {
		t.Errorf("body = %q", records[0].Body)
	}
	if records[0].Version != "1001" {
		t.Errorf("version = %q, want the object generation", records[0].Version)
	}
}

func TestGCS_StatAndLocation(t *testing.T) {
	ctx := context.Background()
	s, _ := newFakeGCSStore(t)

	if _, exists, err := s.Stat(ctx, "team/nope.md"); err != nil || exists {
		t.Fatalf("Stat of a missing object: exists=%v err=%v", exists, err)
	}
	v, err := s.Put(ctx, "team/note.md", []byte("x"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}
	got, exists, err := s.Stat(ctx, "team/note.md")
	if err != nil || !exists || got != v {
		t.Fatalf("Stat = %q exists=%v err=%v, want %q", got, exists, err, v)
	}
	if want := "gs://bucket/kb/memory/team/note.md"; s.Location("team/note.md") != want {
		t.Errorf("Location = %q, want %q", s.Location("team/note.md"), want)
	}
	if want := "gcs://bucket/kb/memory/"; s.Describe() != want {
		t.Errorf("Describe = %q, want %q", s.Describe(), want)
	}
}

func TestGCS_ConcurrentCreatesProduceExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	s, _ := newFakeGCSStore(t)

	const writers = 16
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, err := s.Put(ctx, "team/hot.md", []byte(strings.Repeat("x", i+1)), CreateOnly()); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1", wins)
	}
}

func TestGCS_NormalisePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"kb/memory":  "kb/memory/",
		"kb/memory/": "kb/memory/",
		"/kb/memory": "kb/memory/",
		"":           "",
	} {
		if got := normalisePrefix(in); got != want {
			t.Errorf("normalisePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
