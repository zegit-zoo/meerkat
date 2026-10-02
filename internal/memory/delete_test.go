package memory

import (
	"context"
	"testing"
)

// delete_test.go pins the optional Deleter on every backend: a live
// document is removed, a missing one is not an error, and an unsafe key
// is refused before any backend call.
func TestDeleter_EveryBackend(t *testing.T) {
	ctx := context.Background()
	s3store, _ := newFakeS3Store(t)
	stores := map[string]Store{
		"local": newLocal(t),
		"gcs":   newGCSStoreWithAPI(newFakeGCS(), "bucket", "kb/memory/"),
		"s3":    s3store,
	}
	for name, s := range stores {
		t.Run(name, func(t *testing.T) {
			d, ok := s.(Deleter)
			if !ok {
				t.Fatalf("%T does not implement Deleter", s)
			}
			if _, err := s.Put(ctx, "parked/one.md", []byte("# needs-human\n"), CreateOnly()); err != nil {
				t.Fatal(err)
			}
			if err := d.Delete(ctx, "parked/one.md"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, exists, err := s.Stat(ctx, "parked/one.md"); err != nil || exists {
				t.Fatalf("after delete: exists=%v err=%v", exists, err)
			}
			if err := d.Delete(ctx, "parked/one.md"); err != nil {
				t.Errorf("deleting a missing document is not an error: %v", err)
			}
			// The key is free again: a create-only write succeeds.
			if _, err := s.Put(ctx, "parked/one.md", []byte("# needs-human\nagain\n"), CreateOnly()); err != nil {
				t.Errorf("re-create after delete: %v", err)
			}
			for _, bad := range []string{"", "../escape.md", "/abs.md", "not-markdown"} {
				if err := d.Delete(ctx, bad); err == nil {
					t.Errorf("Delete(%q) must be refused", bad)
				}
			}
		})
	}
}
