package ingest

import (
	"path/filepath"
	"testing"
)

func TestIsPathWithinBase(t *testing.T) {
	base := t.TempDir()

	inside := filepath.Join(base, "wiki", "policies", "foo.md")
	if !isPathWithinBase(base, inside) {
		t.Fatalf("expected inside path to be accepted: %s", inside)
	}

	escape := filepath.Join(base, "..", "outside.md")
	if isPathWithinBase(base, escape) {
		t.Fatalf("expected escaping path to be rejected: %s", escape)
	}
}
