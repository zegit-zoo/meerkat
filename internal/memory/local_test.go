package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// local_test.go covers the local backend's two guarantees: a write is
// atomic, and a write is compare-and-swap.

func newLocal(t *testing.T) *LocalStore {
	t.Helper()
	s, err := OpenLocal(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLocal_CreateThenUpdate(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	v1, err := s.Put(ctx, "team/note.md", []byte("one"), CreateOnly())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v1 == "" {
		t.Fatal("create returned an empty version")
	}

	v2, err := s.Put(ctx, "team/note.md", []byte("two"), UpdateFrom(v1))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if v2 == v1 {
		t.Error("version did not change after an update")
	}

	got, exists, err := s.Stat(ctx, "team/note.md")
	if err != nil || !exists {
		t.Fatalf("Stat: %v exists=%v", err, exists)
	}
	if got != v2 {
		t.Errorf("Stat = %q, want %q", got, v2)
	}
}

func TestLocal_PreconditionFailures(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	v1, err := s.Put(ctx, "team/note.md", []byte("one"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}

	// Create over something that exists.
	_, err = s.Put(ctx, "team/note.md", []byte("clobber"), CreateOnly())
	assertConflict(t, err, v1)

	// Update from a version that is no longer current.
	if _, err := s.Put(ctx, "team/note.md", []byte("two"), UpdateFrom(v1)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, "team/note.md", []byte("three"), UpdateFrom(v1))
	assertConflict(t, err, "")

	// Update something that isn't there: a conflict, not a create.
	// Silently creating it would resurrect a memory somebody removed.
	_, err = s.Put(ctx, "team/gone.md", []byte("x"), UpdateFrom("deadbeefdeadbeef"))
	assertConflict(t, err, "")

	// The failed writes changed nothing.
	body, rerr := os.ReadFile(filepath.Join(s.dir, "team", "note.md"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != "two" {
		t.Errorf("stored body = %q, want %q — a refused write leaked through", body, "two")
	}
}

// assertConflict checks err is a *ConflictError. wantCurrent, when
// non-empty, must be the version it reports.
func assertConflict(t *testing.T, err error, wantCurrent Version) {
	t.Helper()
	if err == nil {
		t.Fatal("write succeeded, want a conflict")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want it to wrap ErrConflict", err)
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want a *ConflictError", err)
	}
	if wantCurrent != "" && conflict.Current != wantCurrent {
		t.Errorf("conflict.Current = %q, want %q", conflict.Current, wantCurrent)
	}
}

func TestLocal_ConcurrentCreatesProduceExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	const writers = 16
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		wins      int
		conflicts int
		other     []error
	)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Put(ctx, "team/hot.md", []byte(strings.Repeat("x", i+1)), CreateOnly())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1 — a create race must not be won twice", wins)
	}
	if conflicts != writers-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, writers-1)
	}
}

func TestLocal_ConcurrentUpdatesFromTheSameVersionProduceOneWinner(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	base, err := s.Put(ctx, "team/hot.md", []byte("base"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}

	const writers = 12
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
			// Every writer read the SAME base version. Only one of them
			// may win; the rest must be told their view is stale rather
			// than having their write silently lose.
			if _, err := s.Put(ctx, "team/hot.md", []byte(strings.Repeat("y", i+1)), UpdateFrom(base)); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1 — a lost update slipped through", wins)
	}
}

func TestLocal_LoadSkipsStagingAndNonMarkdown(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	if _, err := s.Put(ctx, "team/live.md", []byte("live"), CreateOnly()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "personal/ns/mine.md", []byte("mine"), CreateOnly()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(ctx, StagingPrefix+"/team/ns/proposal.md", []byte("pending")); err != nil {
		t.Fatal(err)
	}
	// A stray non-markdown file must not be loaded as a page.
	if err := os.WriteFile(filepath.Join(s.dir, "README.txt"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}

	records, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var keys []string
	for _, r := range records {
		keys = append(keys, r.Key)
	}
	want := []string{"personal/ns/mine.md", "team/live.md"}
	if len(keys) != len(want) {
		t.Fatalf("Load keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("Load keys = %v, want %v (sorted)", keys, want)
		}
	}
	for _, k := range keys {
		if strings.HasPrefix(k, StagingPrefix) {
			t.Errorf("Load returned a staged document %q — staged memories must never become readable", k)
		}
	}
}

func TestStaged_MatchesAnySegment(t *testing.T) {
	for key, want := range map[string]bool{
		"_staging/team/ns/p.md":  true,
		"_staging":               true,
		"team/_staging/p.md":     true, // meerkat never writes this; an operator might
		"team/live.md":           false,
		"personal/ns/staging.md": false,
		"personal/_stagingx.md":  false,
	} {
		if got := Staged(key); got != want {
			t.Errorf("Staged(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestLocal_LoadSkipsANestedStagingDirectory(t *testing.T) {
	// Defence in depth: meerkat only ever writes _staging/ at the root,
	// but an operator reorganising the store could produce a nested one,
	// and publishing an unreviewed proposal is the failure that matters.
	ctx := context.Background()
	s := newLocal(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "team", StagingPrefix), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "team", StagingPrefix, "p.md"), []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Errorf("Load returned %+v, want nothing — a nested staging directory was published", records)
	}
}

func TestLocal_StageIsUnconditionalButConfinedToTheStagingPrefix(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	loc, err := s.Stage(ctx, StagingPrefix+"/team/ns/p.md", []byte("first"))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !strings.Contains(loc, StagingPrefix) {
		t.Errorf("Stage location = %q, want it under %s", loc, StagingPrefix)
	}
	// A second proposal for the same key supersedes rather than
	// colliding.
	if _, err := s.Stage(ctx, StagingPrefix+"/team/ns/p.md", []byte("second")); err != nil {
		t.Fatalf("re-stage: %v", err)
	}

	// Staging outside the staging prefix is refused: it is the one
	// place a bug could turn "pending review" into "published".
	if _, err := s.Stage(ctx, "team/sneaky.md", []byte("x")); err == nil {
		t.Error("Stage outside the staging prefix succeeded, want an error")
	}
}

func TestLocal_RefusesUnsafeKeysAndOversizedBodies(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	for _, key := range []string{
		"", "/abs/x.md", "../escape.md", "team/../../escape.md",
		`team\note.md`, "team/note", "team//note.md", "team/./note.md",
	} {
		if _, err := s.Put(ctx, key, []byte("x"), CreateOnly()); err == nil {
			t.Errorf("Put(%q) succeeded, want a rejected key", key)
		}
	}
	if _, err := s.Put(ctx, "team/empty.md", nil, CreateOnly()); err == nil {
		t.Error("Put with an empty body succeeded, want an error")
	}
	big := make([]byte, maxDocumentBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := s.Put(ctx, "team/big.md", big, CreateOnly()); err == nil {
		t.Error("Put over the size cap succeeded, want an error")
	}
	// A zero precondition — neither create-only nor an update — is
	// refused: there is no unconditional overwrite.
	if _, err := s.Put(ctx, "team/note.md", []byte("x"), Precondition{}); err == nil {
		t.Error("Put with no precondition succeeded, want an error")
	}
	if _, err := s.Put(ctx, "team/note.md", []byte("x"), Precondition{Absent: true, Version: "v"}); err == nil {
		t.Error("Put with two preconditions succeeded, want an error")
	}
}

func TestLocal_WriteLeavesNoTemporaryFileBehind(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)
	if _, err := s.Put(ctx, "team/note.md", []byte("body"), CreateOnly()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "team"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "note.md" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just note.md — the temp file was not renamed away", names)
	}
}

func TestLocal_ByteIdenticalRewriteKeepsTheVersion(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)
	v1, err := s.Put(ctx, "team/note.md", []byte("same"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.Put(ctx, "team/note.md", []byte("same"), UpdateFrom(v1))
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Errorf("version changed on a no-op rewrite: %q -> %q; nothing changed, so nobody's precondition should be invalidated", v1, v2)
	}
}
