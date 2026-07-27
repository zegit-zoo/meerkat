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
	// Same rationale as installStaged's copyFileToNewTemp in
	// install.go: the staging path must not be a fixed, guessable
	// name, because a pre-planted symlink there would make `sudo cp`
	// write through it as root — this is the local-privilege-
	// escalation variant of the copyFile symlink-follow bug. We can't
	// call os.CreateTemp here (this process may not have write access
	// to currentExe's directory at all, which is exactly why sudo is
	// needed), so instead we pick an unguessable name ourselves and
	// have the privileged `cp` create it fresh. The backup path keeps
	// its fixed name: it's only ever written via `mv`/rename, which
	// replaces the directory entry itself rather than following it,
	// so a pre-planted symlink there can't be written through.
	suffix, err := randomHexSuffix()
	if err != nil {
		return fmt.Errorf("generate staging suffix: %w", err)
	}
	stagingPath := currentExe + ".new-" + suffix
	backupPath := currentExe + ".old"

	if err := runCommand("sudo", "cp", stagedPath, stagingPath); err != nil {
		return fmt.Errorf("sudo cp staged binary: %w", err)
	}
	if err := runCommand("sudo", "chmod", "0755", stagingPath); err != nil {
		_ = runCommand("sudo", "rm", "-f", stagingPath)
		return fmt.Errorf("sudo chmod staged binary: %w", err)
	}
	if err := runCommand("sudo", "mv", currentExe, backupPath); err != nil {
		_ = runCommand("sudo", "rm", "-f", stagingPath)
		return fmt.Errorf("sudo backup current binary: %w", err)
	}
	if err := runCommand("sudo", "mv", stagingPath, currentExe); err != nil {
		_ = runCommand("sudo", "mv", backupPath, currentExe)
		return fmt.Errorf("sudo install new binary: %w", err)
	}
	_ = runCommand("sudo", "rm", "-f", backupPath)
	return nil
}

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
	if err := syscall.Exec(currentExe, append([]string{currentExe}, os.Args[1:]...), os.Environ()); err != nil {
		return fmt.Errorf("re-exec: %w (the upgrade succeeded — run `mk version` to confirm)", err)
	}
	return nil // unreachable on success
}
