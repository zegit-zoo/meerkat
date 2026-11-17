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

// leaseHelperEnv names the version directory TestLeaseHelperProcess
// leases when the test binary is re-executed as a reader.
const leaseHelperEnv = "MEERKAT_TEST_LEASE_HELPER_DIR"

// TestLeaseHelperProcess is not a test: re-executed by
// TestCacheLease_CrashedReaderDoesNotBlockPruning, it leases a version,
// says so, and sleeps until it is killed.
func TestLeaseHelperProcess(t *testing.T) {
	dir := os.Getenv(leaseHelperEnv)
	if dir == "" {
		t.Skip("helper process for TestCacheLease_CrashedReaderDoesNotBlockPruning")
	}
	if _, err := acquireCacheLease(dir); err != nil {
		_, _ = os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("leased\n")
	time.Sleep(time.Minute)
	os.Exit(3)
}

// The lease is held across processes, and a reader that dies (SIGKILL,
// no cleanup) stops holding it: the kernel drops the lock with the
// process, so no stale-lease timeout is needed.
func TestCacheLease_CrashedReaderDoesNotBlockPruning(t *testing.T) {
	dirs := leaseVersions(t, 3)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), leaseHelperEnv+"="+dirs[0])
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "leased" {
		t.Fatalf("helper said %q, %v", line, err)
	}

	pruneCacheVersions(dirs[2], 1)
	if !exists(dirs[0]) {
		t.Fatal("a version another live process leases was pruned")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	pruneCacheVersions(dirs[2], 1)
	if exists(dirs[0]) {
		t.Error("a lease left by a killed reader still blocks pruning")
	}
	if h := listHidden(t, filepath.Dir(dirs[0])); len(h) != 0 {
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
