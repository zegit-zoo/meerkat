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

	if len(got) != 5 {
		t.Fatalf("expected 5 sudo commands, got %d: %v", len(got), got)
	}

	// The staging path is now an unguessable "<current>.new-<hex>"
	// rather than the fixed "<current>.new" (see
	// TestInstallStagedWithSudo_StagingPathIsUnpredictable for the
	// dedicated regression test on that). Assert the shape here
	// without hardcoding the random suffix, and pull the actual
	// staging path out of the first command so the rest of the
	// sequence can be checked for internal consistency.
	wantPrefixes := []string{
		"sudo cp " + staged + " " + current + ".new-",
		"sudo chmod 0755 " + current + ".new-",
		"sudo mv " + current + " " + current + ".old",
		"sudo mv " + current + ".new-",
		"sudo rm -f " + current + ".old",
	}
	for i, wantPrefix := range wantPrefixes {
		if !strings.HasPrefix(got[i], wantPrefix) {
			t.Errorf("command[%d] = %q, want prefix %q", i, got[i], wantPrefix)
		}
	}
	// Guard against a regression back to the fixed, pre-planting-
	// prone name: neither the cp nor the chmod command may target
	// exactly "<current>.new" (as opposed to "<current>.new-<hex>").
	legacyStagingPath := current + ".new"
	if got[0] == "sudo cp "+staged+" "+legacyStagingPath {
		t.Errorf("cp command uses the old fixed/predictable staging path: %q", got[0])
	}
	if got[1] == "sudo chmod 0755 "+legacyStagingPath {
		t.Errorf("chmod command uses the old fixed/predictable staging path: %q", got[1])
	}

	// The exact same (random) staging path must be used consistently
	// across cp, chmod, and the final promoting mv.
	cpFields := strings.Fields(got[0])
	stagingPath := cpFields[len(cpFields)-1]
	if stagingPath == legacyStagingPath {
		t.Fatalf("staging path used by cp is the old fixed name: %q", stagingPath)
	}
	if !strings.HasSuffix(got[1], " "+stagingPath) {
		t.Errorf("chmod command %q does not target the cp staging path %q", got[1], stagingPath)
	}
	// got[3] is "sudo mv <stagingPath> <current>" — the staging path
	// is the source (middle field), not the last one.
	wantMv := "sudo mv " + stagingPath + " " + current
	if got[3] != wantMv {
		t.Errorf("final mv command = %q, want %q", got[3], wantMv)
	}
}

// TestInstallStagedWithSudo_StagingPathIsUnpredictable is the direct
// regression test for the sudo-path half of the symlink-preplanting
// finding: `sudo cp` writes to the staging path exactly like copyFile
// used to, so if that name were still fixed, a symlink planted ahead
// of time at "<currentExe>.new" would make root write through it (the
// privilege-escalation variant from the audit). Confirm the staging
// path differs across independent invocations, which a fixed name
// never would.
func TestInstallStagedWithSudo_StagingPathIsUnpredictable(t *testing.T) {
	oldRunCommand := runCommand
	t.Cleanup(func() { runCommand = oldRunCommand })

	var stagingPaths []string
	runCommand = func(name string, args ...string) error {
		if name == "sudo" && len(args) >= 3 && args[0] == "cp" {
			stagingPaths = append(stagingPaths, args[2])
		}
		return nil
	}

	staged := filepath.Join(t.TempDir(), "meerkat")
	current := filepath.Join(t.TempDir(), "bin", "meerkat")

	const runs = 3
	for i := 0; i < runs; i++ {
		if err := installStagedWithSudo(staged, current); err != nil {
			t.Fatalf("installStagedWithSudo run %d: %v", i, err)
		}
	}

	if len(stagingPaths) != runs {
		t.Fatalf("expected %d captured staging paths, got %d: %v", runs, len(stagingPaths), stagingPaths)
	}
	seen := make(map[string]bool, runs)
	for _, p := range stagingPaths {
		if p == current+".new" {
			t.Fatalf("staging path is the old fixed/predictable name: %q", p)
		}
		if seen[p] {
			t.Fatalf("staging path %q was reused across invocations — not unpredictable", p)
		}
		seen[p] = true
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
