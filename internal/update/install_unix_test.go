//go:build !windows

package update

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCheckInstallDirWritable_PermissionDenied_Unix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot exercise this path as root")
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(exe, []byte("not-a-binary"), 0o755); err != nil {
		t.Fatalf("seed exe: %v", err)
	}
	// Strip write+execute from the dir so the user can't even
	// create the probe file inside it.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	// Restore so t.TempDir cleanup can run.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := checkInstallDirWritable(exe)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "permission denied") {
		t.Fatalf("expected friendly 'permission denied' message, got: %v", err)
	}
	for _, want := range []string{
		"sudo mk update",
		"in-place atomic swap",
		"user-writable directory",
		dir,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected message to contain %q, got:\n%s", want, msg)
		}
	}
}

func TestInstallStagedWithSudoUsesOnlyFinalCopyMoveCommands_Unix(t *testing.T) {
	oldRunCommand := runCommand
	t.Cleanup(func() { runCommand = oldRunCommand })

	var got []string
	runCommand = func(name string, args ...string) error {
		got = append(got, name+" "+strings.Join(args, " "))
		return nil
	}

	staged := filepath.Join(t.TempDir(), "meerkat")
	current := filepath.Join(t.TempDir(), "bin", "meerkat")
	if err := installStagedWithSudo(staged, current); err != nil {
		t.Fatalf("installStagedWithSudo: %v", err)
	}

	want := []string{
		"sudo cp " + staged + " " + current + ".new",
		"sudo chmod 0755 " + current + ".new",
		"sudo mv " + current + " " + current + ".old",
		"sudo mv " + current + ".new " + current,
		"sudo rm -f " + current + ".old",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sudo command sequence mismatch\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestPermissionDeniedError_MessageShape_Unix is a snapshot of what
// the user sees on stderr when 'mk update' refuses because the
// install dir isn't writable on a Unix host. Kept as a separate test
// so accidental wording regressions are easy to spot.
func TestPermissionDeniedError_MessageShape_Unix(t *testing.T) {
	err := permissionDeniedError("/usr/local/bin/meerkat", "stage", syscall.EACCES)
	msg := err.Error()
	t.Logf("rendered message:\n%s", msg)
	for _, want := range []string{
		"permission denied writing to",
		"in-place atomic swap",
		"sudo mk update",
		"cosign-signature-verified",
		"/usr/local/bin",
		"docs/INSTALL.md#updating",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in error message", want)
		}
	}
}
