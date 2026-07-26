//go:build windows

package update

import (
	"strings"
	"syscall"
	"testing"
)

// TestPermissionDeniedError_MessageShape_Windows is a snapshot of
// what the user sees on stderr when 'mk update' refuses because the
// install dir isn't writable on Windows. Wording differs from Unix:
// no sudo, mentions elevated PowerShell + %LOCALAPPDATA%.
func TestPermissionDeniedError_MessageShape_Windows(t *testing.T) {
	err := permissionDeniedError(`C:\Program Files\meerkat\meerkat.exe`, "stage", syscall.EACCES)
	msg := err.Error()
	t.Logf("rendered message:\n%s", msg)
	for _, want := range []string{
		"permission denied writing to",
		"elevated PowerShell",
		"cosign-signature-verified",
		"setx PATH",
		"docs/INSTALL.md#install-on-windows",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in error message", want)
		}
	}
	// Friendly message should NOT carry the Unix-specific wording.
	for _, antiWant := range []string{
		"sudo mk update",
		"in-place atomic swap", // Unix-only phrase
	} {
		if strings.Contains(msg, antiWant) {
			t.Errorf("did not expect Unix-only phrase %q in Windows message", antiWant)
		}
	}
}

// TestInstallStagedWithSudo_ReturnsBugErrorOnWindows ensures the
// Windows stub of installStagedWithSudo loudly refuses to run rather
// than silently doing something half-correct. The caller in
// SwapAndReExec routes to permissionDeniedError on Windows; this
// stub is only a safety net.
func TestInstallStagedWithSudo_ReturnsBugErrorOnWindows(t *testing.T) {
	err := installStagedWithSudo(`C:\tmp\meerkat.new`, `C:\Program Files\meerkat\meerkat.exe`)
	if err == nil {
		t.Fatal("expected installStagedWithSudo to refuse on Windows")
	}
	if !strings.Contains(err.Error(), "bug") {
		t.Fatalf("expected bug-marker in error, got: %v", err)
	}
}
