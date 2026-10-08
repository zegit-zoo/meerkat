package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// quota_test.go: no single owner can grow a store without bound
// (meerkat-mob#44).

func TestQuotaPartition(t *testing.T) {
	tests := []struct {
		key    string
		prefix string
		shared bool
		ok     bool
	}{
		{"personal/ns1/a.md", "personal/ns1/", false, true},
		{"_staging/team/ns1/a.md", "_staging/team/ns1/", false, true},
		{"_staging/global/ns2/a.md", "_staging/global/ns2/", false, true},
		{"raw/ns1/2026-10-07/id/page.md", "raw/ns1/2026-10-07/", false, true},
		{"team/a.md", "team/", true, true},
		{"global/a.md", "global/", true, true},
		{"done/id.md", "", false, false},
		{"parked/id.md", "", false, false},
		{"staged/kb/id.md", "", false, false},
		{"personal/a.md", "", false, false},
	}
	for _, tc := range tests {
		prefix, shared, ok := quotaPartition(tc.key)
		if prefix != tc.prefix || shared != tc.shared || ok != tc.ok {
			t.Errorf("quotaPartition(%q) = %q, %v, %v; want %q, %v, %v", tc.key, prefix, shared, ok, tc.prefix, tc.shared, tc.ok)
		}
	}
}

func TestQuotaAdmit(t *testing.T) {
	q := Quota{Documents: 2, Bytes: 100, SharedDocuments: 3, SharedBytes: 1000}
	two := []storedObject{{Key: "personal/ns/a.md", Size: 40}, {Key: "personal/ns/b.md", Size: 40}}
	tests := []struct {
		name string
		key  string
		size int
		used []storedObject
		want string // "" admits; else the QuotaError.What
	}{
		{"first document", "personal/ns/a.md", 10, nil, ""},
		{"third document over the count", "personal/ns/c.md", 1, two, "documents"},
		{"update in place within bytes", "personal/ns/a.md", 60, two, ""},
		{"update that grows past bytes", "personal/ns/a.md", 61, two, "bytes"},
		{"new document past bytes", "personal/ns/c.md", 101, nil, "bytes"},
		{"shrinking update while over a lowered limit", "personal/ns/a.md", 1,
			[]storedObject{{Key: "personal/ns/a.md", Size: 90}, {Key: "personal/ns/b.md", Size: 90}, {Key: "personal/ns/c.md", Size: 1}}, ""},
		{"shared scope uses the shared limits", "team/c.md", 500, two, ""},
		{"unowned key is never charged", "done/x.md", 1 << 20, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := q.admit(tc.key, tc.size, tc.used)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("admit = %v, want admitted", err)
				}
				return
			}
			var qe *QuotaError
			if !errors.As(err, &qe) || !errors.Is(err, ErrQuotaExceeded) || qe.What != tc.want {
				t.Fatalf("admit = %v, want a %s QuotaError", err, tc.want)
			}
		})
	}
}

func TestQuotaError_NamesNoLocation(t *testing.T) {
	msg := (&QuotaError{What: "documents", Have: 3, Limit: 2}).Error()
	for _, leak := range []string{"personal/", "_staging", "s3://", "gs://", "/"} {
		if strings.Contains(msg, leak) {
			t.Errorf("quota error %q carries %q", msg, leak)
		}
	}
}

// quotaBackends returns one store of each backend with a small quota.
func quotaBackends(t *testing.T) map[string]Store {
	t.Helper()
	q := Quota{Documents: 3, Bytes: 1 << 10, SharedDocuments: 4, SharedBytes: 1 << 12}
	local, err := OpenLocal(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	local.quota = q
	s3 := newS3StoreWithAPI(newFakeS3(), "bucket", "kb/memory/", "")
	s3.quota = q
	gcs := newGCSStoreWithAPI(newFakeGCS(), "bucket", "kb/memory/")
	gcs.quota = q
	return map[string]Store{"local": local, "s3": s3, "gcs": gcs}
}

func TestQuota_EnforcedByEveryBackend(t *testing.T) {
	ctx := context.Background()
	for name, s := range quotaBackends(t) {
		t.Run(name, func(t *testing.T) {
			// Personal: three documents fit, the fourth is refused, and
			// another namespace is unaffected.
			for i := range 3 {
				if _, err := s.Put(ctx, fmt.Sprintf("personal/ns1/n%d.md", i), []byte("note"), CreateOnly()); err != nil {
					t.Fatalf("put %d: %v", i, err)
				}
			}
			if _, err := s.Put(ctx, "personal/ns1/n3.md", []byte("note"), CreateOnly()); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("fourth personal document: %v, want ErrQuotaExceeded", err)
			}
			if _, err := s.Put(ctx, "personal/ns2/n0.md", []byte("note"), CreateOnly()); err != nil {
				t.Fatalf("another namespace was charged for ns1: %v", err)
			}

			// Bytes: one document that alone exceeds the namespace's bytes.
			if _, err := s.Put(ctx, "personal/ns3/big.md", []byte(strings.Repeat("x", 2<<10)), CreateOnly()); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("oversized namespace: %v, want ErrQuotaExceeded", err)
			}

			// Staging: one proposer's area fills, a re-proposal of the same
			// key still supersedes, and another proposer is unaffected.
			for i := range 3 {
				if _, err := s.Stage(ctx, fmt.Sprintf("_staging/team/ns1/p%d.md", i), []byte("proposal")); err != nil {
					t.Fatalf("stage %d: %v", i, err)
				}
			}
			if _, err := s.Stage(ctx, "_staging/team/ns1/p3.md", []byte("proposal")); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("fourth staged proposal: %v, want ErrQuotaExceeded", err)
			}
			if _, err := s.Stage(ctx, "_staging/team/ns1/p0.md", []byte("revised")); err != nil {
				t.Fatalf("re-proposing an existing key: %v", err)
			}
			if _, err := s.Stage(ctx, "_staging/team/ns2/p0.md", []byte("proposal")); err != nil {
				t.Fatalf("another proposer was charged for ns1: %v", err)
			}

			// Shared scope.
			for i := range 4 {
				if _, err := s.Put(ctx, fmt.Sprintf("team/t%d.md", i), []byte("shared"), CreateOnly()); err != nil {
					t.Fatalf("team put %d: %v", i, err)
				}
			}
			if _, err := s.Put(ctx, "team/t4.md", []byte("shared"), CreateOnly()); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("fifth team document: %v, want ErrQuotaExceeded", err)
			}

			// Nothing refused was stored.
			recs, err := s.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range recs {
				if r.Key == "personal/ns1/n3.md" || r.Key == "personal/ns3/big.md" || r.Key == "team/t4.md" {
					t.Errorf("refused write %s was stored", r.Key)
				}
			}
		})
	}
}

func TestSpecOpen_AppliesConfiguredQuota(t *testing.T) {
	spec := &Spec{Type: BackendLocal, Path: filepath.Join(t.TempDir(), "m"), Quota: &Quota{Documents: 1}}
	st, err := spec.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	local := st.(*LocalStore)
	t.Cleanup(func() { _ = local.Close() })
	want := DefaultQuota()
	want.Documents = 1
	if local.quota != want {
		t.Errorf("quota = %+v, want %+v", local.quota, want)
	}
}

// keepRecorder captures the listing filter Load and Fingerprint pass.
type keepRecorder struct {
	*fakeS3
	keeps []func(string) bool
}

func (k *keepRecorder) List(ctx context.Context, b, prefix string, keep func(string) bool) ([]s3MemoryObject, error) {
	k.keeps = append(k.keeps, keep)
	return k.fakeS3.List(ctx, b, prefix, keep)
}

// TestLoad_StagingIsNotCountedAgainstTheCap: the object cap applies to
// what Load and Fingerprint keep, and they keep no staged proposal, so
// proposals can never make the live set fail to list.
func TestLoad_StagingIsNotCountedAgainstTheCap(t *testing.T) {
	ctx := context.Background()
	api := &keepRecorder{fakeS3: newFakeS3()}
	s := newS3StoreWithAPI(api, "bucket", "kb/memory/", "")
	if _, err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fingerprint(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.keeps) != 2 {
		t.Fatalf("%d listings, want 2", len(api.keeps))
	}
	for i, keep := range api.keeps {
		if keep == nil {
			t.Fatalf("listing %d has no filter: staged objects would count against the cap", i)
		}
		if keep("kb/memory/_staging/team/ns/p.md") {
			t.Errorf("listing %d keeps a staged proposal", i)
		}
		if !keep("kb/memory/team/t.md") || !keep("kb/memory/personal/ns/n.md") {
			t.Errorf("listing %d drops a live document", i)
		}
	}
}
