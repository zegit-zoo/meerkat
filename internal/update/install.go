package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// SwapAndReExec atomically replaces the running binary with newPath
// and re-executes it (preserving the original argv beyond argv[0]).
//
// The downloaded and verified binary is first copied into a private
// temp directory owned by the current user. The final install step
// then copies that staged file next to the running executable and
// promotes it with rename/mv.
//
// The old binary is moved to "<currentExe>.old" before the swap so
// that, if anything goes wrong between rename and re-exec, the user
// can recover by running it manually.
//
// If the install directory is not writable by the current user (a
// common situation when meerkat is installed under /usr/local/bin
// or /usr/bin on Linux), only the final copy/move operations are
// retried through sudo on Unix. Windows has no comparable single-
// step elevation path, so the caller is told to re-run from an
// elevated PowerShell or move the install to a user-writable dir.
func SwapAndReExec(newPath string) error {
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	// Resolve symlinks (e.g. /usr/local/bin/mk -> meerkat). On Windows
	// EvalSymlinks is still safe: returns the same path if there are
	// no reparse points.
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	stagedPath, cleanup, err := stageInTemp(newPath)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := installStaged(stagedPath, currentExe); err != nil {
		if isPermission(err) {
			if runtime.GOOS == "windows" {
				// No sudo on Windows; surface the friendly
				// elevation message and stop.
				return permissionDeniedError(currentExe, "install", err)
			}
			fmt.Fprintf(os.Stderr, "mk update: install directory requires elevated privileges; running sudo for the final copy/move only.\n")
			if sudoErr := installStagedWithSudo(stagedPath, currentExe); sudoErr != nil {
				return wrapSudoInstallError(currentExe, sudoErr)
			}
		} else {
			return err
		}
	}

	return reExec(currentExe)
}

func stageInTemp(newPath string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "meerkat-update-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create update temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	binaryName := "meerkat"
	if runtime.GOOS == "windows" {
		binaryName = "meerkat.exe"
	}
	stagedPath := filepath.Join(dir, binaryName)
	if err := copyFile(newPath, stagedPath, 0o755); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("stage update in temp dir: %w", err)
	}
	return stagedPath, cleanup, nil
}

func installStaged(stagedPath, currentExe string) error {
	stagingPath := currentExe + ".new"
	if err := copyFile(stagedPath, stagingPath, 0o755); err != nil {
		return wrapPermissionError(currentExe, "stage", err)
	}
	defer os.Remove(stagingPath) // no-op if already renamed away

	// Move current out of the way (best-effort backup). On Windows,
	// os.Rename works even on a binary that is currently executing —
	// the running process holds an open handle to the old file by
	// inode, not by path. That's how mk can replace itself live.
	backupPath := currentExe + ".old"
	if err := os.Rename(currentExe, backupPath); err != nil {
		return wrapPermissionError(currentExe, "backup current binary", err)
	}

	// Promote the staged binary.
	if err := os.Rename(stagingPath, currentExe); err != nil {
		// Rollback.
		_ = os.Rename(backupPath, currentExe)
		return wrapPermissionError(currentExe, "install new binary", err)
	}

	// Cleanup the backup. We keep it on Windows because in-use
	// executables can't be deleted, but on Unix it's safe.
	if runtime.GOOS != "windows" {
		_ = os.Remove(backupPath)
	}
	return nil
}

var runCommand = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// checkInstallDirWritable returns a friendly error if the directory
// containing currentExe is not writable by the running user. We probe
// by creating + removing a hidden temp file rather than reading the
// stat mode, because numeric mode bits don't account for ACLs, group
// membership, or per-mount overlays (e.g. NFS root_squash, Windows
// inherited ACLs).
func checkInstallDirWritable(currentExe string) error {
	dir := filepath.Dir(currentExe)
	probe, err := os.CreateTemp(dir, ".mk-update-probe-*")
	if err != nil {
		if isPermission(err) {
			return permissionDeniedError(currentExe, "stage", err)
		}
		// Non-permission errors here (e.g. ENOSPC, EROFS) are also
		// fatal but rarer; surface them verbatim with a hint.
		return fmt.Errorf("install directory %q is not writable: %w", dir, err)
	}
	probePath := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probePath)
	return nil
}

// wrapPermissionError converts a permission denied from the swap path
// into the friendly message, and otherwise falls back to a normal
// wrapped error.
func wrapPermissionError(currentExe, stage string, err error) error {
	if isPermission(err) {
		return permissionDeniedError(currentExe, stage, err)
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func wrapSudoInstallError(currentExe string, err error) error {
	return fmt.Errorf(`install with sudo failed for %q

The new binary was downloaded, cosign-signature-verified, and staged
in a user-owned temp directory. Only the final copy/move operations
were run with sudo.

You can retry with:

  mk update

Or move the install to a user-writable directory so future updates work
without sudo.

Underlying error: %w`, currentExe, err)
}

// isPermission detects EACCES / EPERM through Go's normal wrappings,
// including os.PathError. On Windows the equivalent is
// ERROR_ACCESS_DENIED which os.IsPermission catches.
func isPermission(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return true
	}
	return os.IsPermission(err)
}

// permissionDeniedError produces the friendly, actionable message we
// want users to see when meerkat is installed in a non-user-writable
// directory. The advice differs per OS — Unix users see sudo + ~/.local/bin,
// Windows users see "elevated PowerShell" + %LOCALAPPDATA%\Programs\meerkat.
func permissionDeniedError(currentExe, stage string, cause error) error {
	dir := filepath.Dir(currentExe)
	preferred := preferredUserBin()

	if runtime.GOOS == "windows" {
		preferredExe := filepath.Join(preferred, "meerkat.exe")
		preferredMk := filepath.Join(preferred, "mk.exe")
		return fmt.Errorf(`%s: permission denied writing to %q

The 'mk update' command replaces the running binary in its install
directory. That requires write access to that directory — which is
not granted in this install location (likely under %%ProgramFiles%%
or another machine-wide path).

Two ways forward:

  1. Re-run the same command from an elevated PowerShell (one-off):

       # PowerShell run as Administrator
       mk update

     Note: this is the same trust boundary as running 'mk' itself;
     the binary is cosign-signature-verified before any swap.

  2. Move the install to a user-writable directory so future
     updates work without elevation:

       # PowerShell (regular, no admin)
       New-Item -ItemType Directory -Force -Path %q | Out-Null
       Move-Item -Force %q %q
       Copy-Item -Force %q %q

     Then make sure %q is on your PATH:

       setx PATH "$env:PATH;%s"

     (Reopen the shell after setx to pick up the new PATH.)

See: https://github.com/zegit-zoo/meerkat/blob/main/docs/INSTALL.md#install-on-windows

Underlying error: %w`,
			stage,
			dir,
			preferred,
			currentExe, preferredExe,
			preferredExe, preferredMk,
			preferred,
			preferred,
			cause,
		)
	}

	return fmt.Errorf(`%s: permission denied writing to %q

The 'mk update' command performs an in-place atomic swap of the
running binary. That requires write access to the directory
holding the binary — which is not granted in this install
location (likely root-owned).

Two ways forward:

  1. Re-run with elevated privileges (one-off):

       sudo mk update

     Note: this is the same trust boundary as running 'mk' itself;
     the binary is cosign-signature-verified before any swap.

  2. Move the install to a user-writable directory so future
     updates work without sudo:

       sudo mv %q %q
       sudo rm -f %q
       ln -sf meerkat %q

     Make sure the destination directory is on your $PATH.

See: https://github.com/zegit-zoo/meerkat/blob/main/docs/INSTALL.md#updating

Underlying error: %w`,
		stage,
		dir,
		currentExe,
		filepath.Join(preferred, "meerkat"),
		filepath.Join(dir, "mk"),
		filepath.Join(preferred, "mk"),
		cause,
	)
}

// preferredUserBin returns the user-writable bin directory we
// recommend as the move-to target.
//
//   - macOS / Linux:  $HOME/.local/bin
//   - Windows:        %LOCALAPPDATA%\Programs\meerkat
//
// Hardcoded by OS; we don't probe at runtime because we want the
// recommendation stable regardless of whether the directory already
// exists.
func preferredUserBin() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "Programs", "meerkat")
		}
		// Fall back to %APPDATA% / userprofile if LOCALAPPDATA is
		// somehow not set (e.g. minimal container image).
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = "%USERPROFILE%"
		}
		return filepath.Join(home, "AppData", "Local", "Programs", "meerkat")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "$HOME"
	}
	return filepath.Join(home, ".local", "bin")
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(mode)
}
