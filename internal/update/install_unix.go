//go:build !windows

package update

import (
	"fmt"
	"os"
	"syscall"
)

// installStagedWithSudo retries the final copy+rename steps under
// sudo. The downloaded binary has already been verified and staged
// in a user-owned temp dir; only the operations that touch the
// install directory itself need elevation.
//
// Unix-only: Windows has no single-step user-driven elevation
// equivalent. There the caller falls back to telling the user to
// re-run from an elevated PowerShell.
func installStagedWithSudo(stagedPath, currentExe string) error {
	// mk update only needs the backup transiently, so it gets an
	// unguessable name: nothing can have been pre-planted there.
	suffix, err := randomHexSuffix()
	if err != nil {
		return fmt.Errorf("generate backup suffix: %w", err)
	}
	backupPath, err := swapWithBackupSudoAt(stagedPath, currentExe, currentExe+".old-"+suffix)
	if err != nil {
		return err
	}
	_ = runCommand("sudo", "rm", "-f", "--", backupPath)
	return nil
}

// swapWithBackupSudo is swapWithBackup's sudo-elevated twin: same
// stage/backup/promote sequence, but every filesystem operation that
// touches currentExe's directory runs through `sudo` because this
// process itself doesn't have write access there. On success the
// backup is left at the returned path rather than removed —
// installStagedWithSudo removes it immediately (mirroring
// installStaged's non-Windows behavior, since mk update's binary was
// already verified before the swap ran); InstallAtomic in
// bootstrap.go keeps it until its own post-install check passes.
func swapWithBackupSudo(stagedPath, currentExe string) (backupPath string, err error) {
	// This variant keeps the stable "<exe>.old" name because
	// meerkat-bootstrap's RemoveBackup/RestoreBackup look for it.
	// `mv f link-to-dir` would move f *into* the directory a planted
	// symlink points at, so the name is first cleared with `rm -f`
	// (which unlinks a symlink without following it, and refuses a real
	// directory) before the move.
	backupPath = currentExe + ".old"
	if err := runCommand("sudo", "rm", "-f", "--", backupPath); err != nil {
		return "", fmt.Errorf("sudo clear backup path: %w", err)
	}
	return swapWithBackupSudoAt(stagedPath, currentExe, backupPath)
}

// swapWithBackupSudoAt does the sudo stage/backup/promote sequence with
// the backup at backupPath. Every mv/cp/rm passes "--" so no path can
// be read as an option, and callers choose a backupPath that cannot be
// a pre-planted directory symlink (random name, or cleared first).
func swapWithBackupSudoAt(stagedPath, currentExe, backupPath string) (string, error) {
	// Same rationale as installStaged's copyFileToNewTemp in
	// install.go: the staging path must not be a fixed, guessable
	// name, because a pre-planted symlink there would make `sudo cp`
	// write through it as root — this is the local-privilege-
	// escalation variant of the copyFile symlink-follow bug. We can't
	// call os.CreateTemp here (this process may not have write access
	// to currentExe's directory at all, which is exactly why sudo is
	// needed), so instead we pick an unguessable name ourselves and
	// have the privileged `cp` create it fresh.
	suffix, err := randomHexSuffix()
	if err != nil {
		return "", fmt.Errorf("generate staging suffix: %w", err)
	}
	stagingPath := currentExe + ".new-" + suffix

	if err := runCommand("sudo", "cp", "--", stagedPath, stagingPath); err != nil {
		return "", fmt.Errorf("sudo cp staged binary: %w", err)
	}
	if err := runCommand("sudo", "chmod", "0755", "--", stagingPath); err != nil {
		_ = runCommand("sudo", "rm", "-f", "--", stagingPath)
		return "", fmt.Errorf("sudo chmod staged binary: %w", err)
	}
	if err := runCommand("sudo", "mv", "-f", "--", currentExe, backupPath); err != nil {
		_ = runCommand("sudo", "rm", "-f", "--", stagingPath)
		return "", fmt.Errorf("sudo backup current binary: %w", err)
	}
	if err := runCommand("sudo", "mv", "-f", "--", stagingPath, currentExe); err != nil {
		_ = runCommand("sudo", "mv", "-f", "--", backupPath, currentExe)
		return "", fmt.Errorf("sudo install new binary: %w", err)
	}
	return backupPath, nil
}

// execFn is syscall.Exec; a var so tests can observe the hand-off
// without replacing the test process.
var execFn = syscall.Exec

// reExec replaces the current process image with the newly-installed
// binary using syscall.Exec, so the same shell session immediately
// sees the new version. Unix-only — Windows uses a child-process +
// exit pattern instead, see install_windows.go.
func reExec(currentExe string) error {
	// #nosec G702 -- gosec flags currentExe as a "tainted command"
	// source because it came from os.Executable(). Here that path
	// is *our own binary* (the one we just installed); an attacker
	// who could rewrite it could just as easily replace the binary
	// itself and skip the syscall.Exec round-trip. The taint is
	// real but not exploitable in this context.
	if err := execFn(currentExe, reExecArgv(currentExe), reExecEnv(os.Environ())); err != nil {
		return fmt.Errorf("re-exec: %w (the upgrade succeeded — run `mk version` to confirm)", err)
	}
	return nil // unreachable on success
}
