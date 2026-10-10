package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The batch file holds every rendered prompt: owner-only, also when it
// already existed with wider permissions (meerkat-mob#35).
func TestCreateBatchFile_OwnerOnly(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.jsonl")
	existing := filepath.Join(dir, "existing.jsonl")
	if err := os.WriteFile(existing, []byte("old content"), 0o644); err != nil { //nolint:gosec // G306: the test needs a world-readable file to narrow.
		t.Fatal(err)
	}
	for _, name := range []string{fresh, existing} {
		f, err := createBatchFile(name)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		fi, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 || fi.Size() != 0 {
			t.Errorf("%s: mode %v size %d; want 0600 and truncated", filepath.Base(name), fi.Mode().Perm(), fi.Size())
		}
	}
}
