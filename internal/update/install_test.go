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
