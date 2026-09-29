//go:build unix

package gitinfo

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO planted at .git/HEAD is refused, not opened: opening one blocks
// until a writer appears, which would stall the probe (review N4).
func TestHead_RefusesAFIFOWithoutBlocking(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	head := filepath.Join(dir, ".git", "HEAD")
	if err := os.Remove(head); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(head, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	r, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := r.Head(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Head read a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Head blocked on a FIFO")
	}
}
