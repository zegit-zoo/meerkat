package cli

import (
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/update"
)

// Under the post-install guard, `update --force --yes` must return
// immediately without any release lookup (this test would hit the
// network, and loop, if the guard were missing).
func TestUpdate_GuardShortCircuits(t *testing.T) {
	t.Setenv(update.UpdatedGuardEnv, "1")
	out, err := execRoot(t, "update", "--force", "--yes")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "not updating again") {
		t.Errorf("output = %q", out)
	}
}
