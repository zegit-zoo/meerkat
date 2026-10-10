package collections

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// memory_degrade_test.go: a memory store that cannot be loaded degrades
// its collection instead of unmounting it (meerkat-mob#44).

// unloadableStore is a local store whose Load always fails, the way an
// unreachable or over-cap store does.
type unloadableStore struct{ *memory.LocalStore }

var errUnloadable = errors.New("store will not load")

func (unloadableStore) Load(context.Context) ([]memory.Record, error) { return nil, errUnloadable }

func TestAttachMemory_UnloadableStoreDegradesButStaysWritable(t *testing.T) {
	ctx := context.Background()
	c := FromPages("notes", []kb.Page{page("handbook/onboarding", "Onboarding", "how we onboard people")})
	local, err := memory.OpenLocal(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	err = c.AttachMemory(ctx, unloadableStore{local})
	if !errors.Is(err, errUnloadable) {
		t.Fatalf("AttachMemory = %v, want the load failure reported", err)
	}
	if c.Memory() == nil {
		t.Fatal("the store was not attached: saves would be refused")
	}
	if _, err := c.Load("handbook/onboarding"); err != nil {
		t.Errorf("content is not served after a failed memory load: %v", err)
	}
	if _, _, err := c.SaveMemory(ctx, "team/after.md", memoryDoc(t, "After", "written after a failed load"), memory.CreateOnly()); err != nil {
		t.Fatalf("SaveMemory after a failed load: %v", err)
	}
	if _, err := c.Load("memory/team/after"); err != nil {
		t.Errorf("a memory saved after a failed load is not served: %v", err)
	}
}

// TestOpen_MemoryStorePastTheCapStillMounts is the end-to-end shape of
// the finding: a store holding more live documents than one Load
// accepts must not stop the collection mounting.
func TestOpen_MemoryStorePastTheCapStillMounts(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 20,001 files")
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"wiki/a.md": "---\nid: a\ntitle: A\n---\nbody a\n"})
	memDir := filepath.Join(t.TempDir(), "memory")
	team := filepath.Join(memDir, "team")
	if err := os.MkdirAll(team, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 20_000; i++ {
		if err := os.WriteFile(filepath.Join(team, fmt.Sprintf("n%05d.md", i)), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	reg, err := Open(context.Background(), []contentsource.ResolvedCollection{
		{Name: "a", Dir: dir, Source: contentsource.Source{
			Type:   "local",
			Layout: contentsource.MergeLayout(contentsource.Layout{}),
			Memory: &memory.Spec{Type: memory.BackendLocal, Path: memDir},
		}},
		{Name: "b", Dir: t.TempDir(), Source: contentsource.Source{Type: "local", Layout: contentsource.MergeLayout(contentsource.Layout{})}},
	})
	if err != nil {
		t.Fatalf("Open failed on a store past the cap: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	c, err := reg.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Load("a"); err != nil {
		t.Errorf("content is not served: %v", err)
	}
	if c.Memory() == nil {
		t.Error("memory store not attached")
	}
}
