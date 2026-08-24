package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// fingerprint_test.go covers the probe half of memory reconciliation:
// the cheap listing digest two replicas compare to decide whether
// anything has changed under a shared GCS memory store.

// TestGCSFingerprint_ChangesExactlyWhenLoadWould is the Fingerprinter
// contract, tested as an if-and-only-if. Both directions matter:
// changing when Load did not would make a fleet reload for nothing,
// forever; NOT changing when Load did would leave replicas permanently
// out of sync with no error anywhere.
func TestGCSFingerprint_ChangesExactlyWhenLoadWould(t *testing.T) {
	ctx := context.Background()
	s, api := newFakeGCSStore(t)

	// A fingerprinter at all — the interface reconciliation dispatches on.
	var fp Fingerprinter = s
	_ = fp

	empty, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if again, _ := s.Fingerprint(ctx); again != empty {
		t.Error("Fingerprint is not stable across calls with no writes")
	}

	v1, err := s.Put(ctx, "team/note.md", []byte("one"), CreateOnly())
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created == empty {
		t.Error("creating a live document did not change the fingerprint")
	}

	if _, err := s.Put(ctx, "team/note.md", []byte("two"), UpdateFrom(v1)); err != nil {
		t.Fatal(err)
	}
	overwritten, err := s.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if overwritten == created {
		t.Error("overwriting a live document did not change the fingerprint — replicas would never see the new body")
	}

	// A STAGED proposal is excluded from Load, so it must be excluded
	// here too: a pending, unreviewed artifact must not make every
	// replica in the fleet reload.
	if _, err := s.Stage(ctx, StagingPrefix+"/team/ns/proposal.md", []byte("pending")); err != nil {
		t.Fatal(err)
	}
	if staged, _ := s.Fingerprint(ctx); staged != overwritten {
		t.Error("staging a proposal moved the fingerprint — a pending artifact must not trigger a fleet-wide reload")
	}

	// Neither does an object under the prefix that is not a memory
	// document at all.
	if err := api.WriteUnconditional(ctx, "bucket", "kb/memory/notes.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if other, _ := s.Fingerprint(ctx); other != overwritten {
		t.Error("a non-markdown object under the prefix moved the fingerprint")
	}

	// And the set it summarises is exactly the set Load returns.
	records, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Key != "team/note.md" {
		t.Fatalf("Load = %+v, want just the one live document", records)
	}
}

// TestGCSFingerprint_TwoStoresOverOneBucketConverge is the replica
// story at the store layer: two processes, one bucket, no shared
// in-process state. A write through one becomes visible to the other by
// way of the fingerprint moving and Load returning it.
func TestGCSFingerprint_TwoStoresOverOneBucketConverge(t *testing.T) {
	ctx := context.Background()
	bucket := newFakeGCS()
	replicaA := newGCSStoreWithAPI(bucket, "example-kb", "kb/memory/")
	replicaB := newGCSStoreWithAPI(bucket, "example-kb", "kb/memory/")

	before, err := replicaB.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replicaA.Put(ctx, "team/deploy.md", []byte("cordon first"), CreateOnly()); err != nil {
		t.Fatalf("replica A write: %v", err)
	}
	after, err := replicaB.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("replica B's fingerprint did not move after replica A wrote — B would never reload")
	}
	records, err := replicaB.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || string(records[0].Body) != "cordon first" {
		t.Fatalf("replica B loaded %+v, want replica A's write", records)
	}
	// And the two agree on the digest, so neither reloads on a schedule
	// the other has already caught up with.
	if a, _ := replicaA.Fingerprint(ctx); a != after {
		t.Error("two stores over the same bucket disagree about the fingerprint")
	}
}

// TestGCSFingerprint_ListFailureSurfaces: a probe that cannot reach the
// bucket must report it, so the cycle is marked degraded rather than
// silently treated as "nothing changed".
func TestGCSFingerprint_ListFailureSurfaces(t *testing.T) {
	s := newGCSStoreWithAPI(&failingListAPI{}, "bucket", "shared/notes/")
	if _, err := s.Fingerprint(context.Background()); err == nil {
		t.Fatal("a failed listing must not be reported as an unchanged store")
	}
}

// failingListAPI is a gcsMemoryAPI whose List always fails.
type failingListAPI struct{ gcsMemoryAPI }

func (f *failingListAPI) List(context.Context, string, string) ([]gcsObject, error) {
	return nil, errFakeNotExist
}

func (f *failingListAPI) Close() error { return nil }

// --- spec validation ---------------------------------------------------

func TestSpec_RefreshRequiresAGCSStore(t *testing.T) {
	every := &refresh.Spec{Interval: refresh.Duration(time.Minute)}

	local := &Spec{Type: BackendLocal, Path: "/srv/memory", Refresh: every}
	err := local.Validate("collections[notes].memory", false)
	if err == nil {
		t.Fatal("a refresh block on a local store should be refused")
	}
	if !strings.Contains(err.Error(), "no other writer") {
		t.Errorf("error = %v, want it to explain why a local store cannot converge", err)
	}

	gcs := &Spec{Type: BackendGCS, Bucket: "b", Prefix: "kb/memory/", Refresh: every}
	if err := gcs.Validate("collections[notes].memory", false); err != nil {
		t.Errorf("a refresh block on a gcs store should be accepted, got %v", err)
	}
}

func TestSpec_RefreshIsValidated(t *testing.T) {
	s := &Spec{
		Type:    BackendGCS,
		Bucket:  "b",
		Prefix:  "kb/memory/",
		Refresh: &refresh.Spec{Interval: refresh.Duration(time.Second)},
	}
	err := s.Validate("collections[notes].memory", false)
	if err == nil {
		t.Fatal("an out-of-range interval should be refused")
	}
	if !strings.Contains(err.Error(), "collections[notes].memory.refresh") {
		t.Errorf("error = %v, want it to name the config path", err)
	}
}
