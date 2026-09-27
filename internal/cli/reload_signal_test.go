//go:build unix

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// controllerReloader is the hosted server's Reload, minus the server: the
// same refresh.Controller.ReloadNow over the registry's targets that
// HostedServer.Reload calls.
type controllerReloader struct{ ctl *refresh.Controller }

func (r controllerReloader) Reload(ctx context.Context) error { return r.ctl.ReloadNow(ctx) }

// lockedBuffer is a bytes.Buffer safe to write from the reload goroutine
// while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestReloadOnSignal_SIGHUPRebuildsALocalCollection is issue #105 end to
// end through a REAL signal: a page written into a `type: local`
// collection after its index was built is unfindable, a SIGHUP is sent
// to this process, and the same registry — no restart, no reopen — then
// finds it.
func TestReloadOnSignal_SIGHUPRebuildsALocalCollection(t *testing.T) {
	var resolved []contentsource.ResolvedCollection
	dirs := map[string]string{}
	for _, name := range []string{"notes", "refs"} {
		dir := t.TempDir()
		dirs[name] = dir
		write(t, filepath.Join(dir, "wiki", name, "stable.md"), "---\nid: "+name+"/stable\ntitle: Stable\n---\n# Stable\n\nLighthouses.\n")
		resolved = append(resolved, contentsource.ResolvedCollection{
			Name: name, Dir: dir, Provenance: "disk:" + dir,
			Source: contentsource.Source{Type: contentsource.TypeLocal, Path: dir, Layout: contentsource.Layout{Wiki: "wiki"}},
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg, err := collections.Open(ctx, resolved)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	search := func(q string) int {
		t.Helper()
		hits, err := reg.Search(ctx, "", q, 10)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		return len(hits)
	}
	if n := search("lighthouses"); n != 2 {
		t.Fatalf("stable pages = %d, want 2", n)
	}
	write(t, filepath.Join(dirs["notes"], "wiki", "notes", "zebrafish.md"), "---\nid: notes/zebrafish\ntitle: Zebrafish\n---\n# Zebrafish\n\nWritten after startup.\n")
	if n := search("zebrafish"); n != 0 {
		t.Fatalf("precondition: the startup index already has the new page")
	}

	// Register exactly as `mk mcp serve-http` does, before the loop runs,
	// so the signal below cannot arrive unhandled.
	sighup := notifyReload()
	out := &lockedBuffer{}
	go reloadOnSignal(ctx, controllerReloader{refresh.New(refresh.Options{Targets: reg.RefreshTargets()})}, out, sighup)

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill -HUP self: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "reload complete") {
		if time.Now().After(deadline) {
			t.Fatalf("no reload after SIGHUP; output: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := search("zebrafish"); n != 1 {
		t.Fatalf("after SIGHUP, zebrafish hits = %d, want 1", n)
	}
	if n := search("lighthouses"); n != 2 {
		t.Errorf("after SIGHUP, stable pages = %d, want 2", n)
	}
}
