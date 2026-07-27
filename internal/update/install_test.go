package update

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestIsPermission(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain io.EOF", errors.New("eof"), false},
		{"raw EACCES", syscall.EACCES, true},
		{"raw EPERM", syscall.EPERM, true},
		{
			"wrapped EACCES via PathError",
			&fs.PathError{Op: "open", Path: "/foo", Err: syscall.EACCES},
			true,
		},
		{
			"wrapped EACCES via fmt.Errorf",
			errors.New(""), // placeholder, replaced below
			true,
		},
	}
	// Replace the placeholder with a properly wrapped EACCES.
	cases[len(cases)-1].err = wrapErrf("stage", syscall.EACCES)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermission(tc.err); got != tc.want {
				t.Fatalf("isPermission(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// wrapErrf is a tiny helper so the test mirrors how the production
// code wraps errors via fmt.Errorf("%w").
func wrapErrf(stage string, cause error) error {
	return wrapPermissionError("/tmp/nope", stage, cause)
}

func TestCheckInstallDirWritable_OK(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(exe, []byte("not-a-binary"), 0o755); err != nil {
		t.Fatalf("seed exe: %v", err)
	}
	if err := checkInstallDirWritable(exe); err != nil {
		t.Fatalf("expected nil for writable dir, got: %v", err)
	}
}

func TestWrapPermissionError_NonPermissionPassesThrough(t *testing.T) {
	cause := errors.New("ENOSPC: out of space")
	err := wrapPermissionError("/usr/local/bin/meerkat", "stage", cause)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	// The "in-place atomic swap" wording is Unix-specific; on Windows
	// the friendly message reads differently. Either way, the friendly
	// text only fires on actual permission errors.
	if isPermission(cause) {
		t.Fatal("ENOSPC should not be classified as a permission error")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("expected error chain to contain cause, got: %v", err)
	}
}

func TestStageInTempCopiesExecutableToTempDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "meerkat.download")
	if err := os.WriteFile(src, []byte("new-binary"), 0o600); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	staged, cleanup, err := stageInTemp(src)
	if err != nil {
		t.Fatalf("stageInTemp: %v", err)
	}
	defer cleanup()

	if filepath.Dir(staged) == filepath.Dir(src) {
		t.Fatalf("staged path should be in a separate temp dir, got %q", staged)
	}
	wantName := "meerkat"
	if runtime.GOOS == "windows" {
		wantName = "meerkat.exe"
	}
	if filepath.Base(staged) != wantName {
		t.Fatalf("staged file name = %q, want %q", filepath.Base(staged), wantName)
	}
	body, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(body) != "new-binary" {
		t.Fatalf("staged file body = %q", body)
	}
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	// Permission bits on Windows are not POSIX; just check the file is
	// writable by the owner.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Fatalf("staged file mode = %o, want 755", info.Mode().Perm())
	}
}

// TestInstallStaged_DoesNotFollowPreplantedSymlinkAtStagingPath is the
// regression test for the copyFile symlink-follow finding: an
// attacker who can write into currentExe's directory (e.g. a shared
// admin-writable install dir) used to be able to pre-plant a symlink
// at the fixed, predictable staging name ("<currentExe>.new") that
// installStaged wrote through via os.OpenFile(..., O_TRUNC). Confirm
// that a symlink planted at that exact legacy path today is left
// completely untouched, and that the real install still succeeds via
// the new unguessable staging name.
func TestInstallStaged_DoesNotFollowPreplantedSymlinkAtStagingPath(t *testing.T) {
	dir := t.TempDir()
	currentExe := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(currentExe, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("seed currentExe: %v", err)
	}

	// The victim file lives outside currentExe's directory, standing
	// in for something like ~/.ssh/authorized_keys in the real-world
	// scenario from the audit.
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("victim-content"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	// Pre-plant a symlink at the OLD fixed/predictable staging path
	// (currentExe + ".new") pointing at the victim. If installStaged
	// ever regresses to writing through that fixed name, this proves
	// it by clobbering the victim file.
	preplanted := currentExe + ".new"
	if err := os.Symlink(victim, preplanted); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	staged := filepath.Join(t.TempDir(), "meerkat.download")
	if err := os.WriteFile(staged, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("seed staged binary: %v", err)
	}

	if err := installStaged(staged, currentExe); err != nil {
		t.Fatalf("installStaged: %v", err)
	}

	// The victim file must be byte-for-byte untouched.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != "victim-content" {
		t.Fatalf("victim file was overwritten via pre-planted symlink: %q", got)
	}

	// The pre-planted symlink itself must still be exactly what we
	// planted: installStaged should never have touched that fixed
	// path at all (not even to remove or replace it).
	fi, err := os.Lstat(preplanted)
	if err != nil {
		t.Fatalf("lstat pre-planted symlink: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pre-planted symlink at %q was replaced with a regular file", preplanted)
	}
	target, err := os.Readlink(preplanted)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != victim {
		t.Fatalf("pre-planted symlink target changed: got %q, want %q", target, victim)
	}

	// And the real install must have gone through, via whatever
	// unguessable name installStaged actually used.
	newContent, err := os.ReadFile(currentExe)
	if err != nil {
		t.Fatalf("currentExe not installed: %v", err)
	}
	if string(newContent) != "new-binary" {
		t.Fatalf("currentExe content = %q, want %q", newContent, "new-binary")
	}

	// No leftover staging file (renamed away) or backup (removed on
	// non-Windows) should remain besides the untouched symlink and
	// the promoted binary.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if runtime.GOOS != "windows" {
		want := map[string]bool{"meerkat": true, "meerkat.new": true}
		if len(names) != len(want) {
			t.Fatalf("unexpected leftover files in install dir: %v", names)
		}
		for _, n := range names {
			if !want[n] {
				t.Errorf("unexpected leftover file %q in install dir: %v", n, names)
			}
		}
	}
}

// TestCopyFileToNewTemp_DoesNotFollowPreplantedSymlink is a focused
// unit test on the new helper itself: pre-plant a symlink at what
// WOULD be a colliding name inside dir, then confirm CreateTemp's
// unguessable naming means copyFileToNewTemp never even considers
// that path, so the symlink is left alone and a fresh file is created
// instead.
func TestCopyFileToNewTemp_DoesNotFollowPreplantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("victim-content"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	// Plant a symlink at the (deliberately non-random) fixed name an
	// old, buggy implementation might have used.
	fixed := filepath.Join(dir, "meerkat.new")
	if err := os.Symlink(victim, fixed); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	path, err := copyFileToNewTemp(src, dir, "meerkat.new-*", 0o755)
	if err != nil {
		t.Fatalf("copyFileToNewTemp: %v", err)
	}
	defer os.Remove(path)

	if path == fixed {
		t.Fatalf("copyFileToNewTemp used the fixed/predictable name %q", fixed)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != "victim-content" {
		t.Fatalf("victim clobbered via pre-planted symlink: %q", got)
	}
	newContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if string(newContent) != "payload" {
		t.Fatalf("new file content = %q, want %q", newContent, "payload")
	}
}

// TestCopyFile_RefusesToFollowExistingSymlink covers copyFile's own
// O_EXCL hardening directly: given a pre-existing symlink at dst (even
// one dangling / pointing nowhere), copyFile must fail rather than
// write through it.
func TestCopyFile_RefusesToFollowExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("victim-content"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	err := copyFile(src, dst, 0o755)
	if err == nil {
		t.Fatal("expected copyFile to refuse an existing symlink destination")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("expected an ErrExist-class error, got: %v", err)
	}
	got, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatalf("read victim: %v", rerr)
	}
	if string(got) != "victim-content" {
		t.Fatalf("victim clobbered via pre-planted symlink: %q", got)
	}
}

func TestPreferredUserBin_NonEmpty(t *testing.T) {
	got := preferredUserBin()
	if got == "" {
		t.Fatal("preferredUserBin returned empty string")
	}
	// The suffix differs per OS; assert the per-OS suffix.
	if runtime.GOOS == "windows" {
		want := filepath.Join("Programs", "meerkat")
		if !strings.HasSuffix(got, want) {
			t.Fatalf("preferredUserBin = %q, want suffix %q", got, want)
		}
	} else {
		want := filepath.Join(".local", "bin")
		if !strings.HasSuffix(got, want) {
			t.Fatalf("preferredUserBin = %q, want suffix %q", got, want)
		}
	}
}
