//go:build unix

package contentsource

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// leaseHelperEnv, set to 1, makes the re-executed test binary act as
// the reader in TestCacheLease_CrashedReaderDoesNotBlockPruning.
const leaseHelperEnv = "MEERKAT_TEST_LEASE_HELPER"

// TestLeaseHelperProcess is not a test: re-executed by
// TestCacheLease_CrashedReaderDoesNotBlockPruning, it writes three
// versions of a location, leases the oldest, prints its path, and sleeps
// until it is killed. It makes the directories itself so that no path
// reaches the cache code from the environment.
func TestLeaseHelperProcess(t *testing.T) {
	if os.Getenv(leaseHelperEnv) != "1" {
		t.Skip("helper process for TestCacheLease_CrashedReaderDoesNotBlockPruning")
	}
	parent, err := os.MkdirTemp("", "meerkat-lease-*")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"v0", "v1", "v2"} {
		d := filepath.Join(parent, name)
		completeEntry(t, d, "# v\n")
		when := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(d, completionMarker), when, when); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := acquireCacheLease(filepath.Join(parent, "v0")); err != nil {
		_, _ = os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("leased " + parent + "\n")
	time.Sleep(time.Minute)
	os.Exit(3)
}

// The lease is held across processes, and a reader that dies (SIGKILL,
// no cleanup) stops holding it: the kernel drops the lock with the
// process, so no stale-lease timeout is needed.
func TestCacheLease_CrashedReaderDoesNotBlockPruning(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), leaseHelperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	parent, ok := strings.CutPrefix(strings.TrimSpace(line), "leased ")
	if err != nil || !ok || !filepath.IsAbs(parent) {
		t.Fatalf("helper said %q, %v", line, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	leased, current := filepath.Join(parent, "v0"), filepath.Join(parent, "v2")

	pruneCacheVersions(current, 1)
	if !exists(leased) {
		t.Fatal("a version another live process leases was pruned")
	}
	if exists(filepath.Join(parent, "v1")) {
		t.Error("an unleased old version was kept")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	pruneCacheVersions(current, 1)
	if exists(leased) {
		t.Error("a lease left by a killed reader still blocks pruning")
	}
	if h := listHidden(t, parent); len(h) != 0 {
		t.Errorf("aside left behind: %v", h)
	}
}

// A cache the process may not write to (a read-only volume) is served
// without a lease rather than refused: nothing can prune it either.
func TestAcquireCacheLease_ReadOnlyCacheServesWithoutALease(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dirs := leaseVersions(t, 1)
	if err := os.Chmod(dirs[0], 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dirs[0], 0o750) })
	l, err := acquireCacheLease(dirs[0])
	if err != nil {
		t.Fatalf("acquireCacheLease on a read-only version: %v", err)
	}
	l.Release()
}
