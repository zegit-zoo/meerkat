//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
)

// installStagedWithSudo is a Unix-only path. On Windows there is no
// equivalent single-step user-driven elevation; the caller must
// surface permissionDeniedError before reaching this path. We keep
// the symbol so the cross-platform call site in install.go compiles,
// but it must never actually run on Windows.
func installStagedWithSudo(_, _ string) error {
	return fmt.Errorf(
		"installStagedWithSudo called on windows — this is a bug; " +
			"the caller should have returned permissionDeniedError instead")
}

// swapWithBackupSudo mirrors installStagedWithSudo's stub: Windows has
// no single-step user-driven elevation equivalent, so InstallAtomic
// (bootstrap.go) must return permissionDeniedError before ever
// reaching this path, exactly like SwapAndReExec already does.
func swapWithBackupSudo(_, _ string) (string, error) {
	return "", fmt.Errorf(
		"swapWithBackupSudo called on windows — this is a bug; " +
			"the caller should have returned permissionDeniedError instead")
}

// reExec launches the freshly-installed binary as a child process
// with the original argv and exits the current process. This is the
// standard Windows self-update pattern: syscall.Exec is unavailable
// on Windows (no fork/exec model), and we can't replace the running
// process image, so we hand off control and quit.
//
// The shell sees the original mk return immediately (exit code 0 on
// success); the new mk runs in the background and writes its output
// to the same stdout/stderr handles. Users will see two prompts —
// one when mk update returns, one when the new mk finishes — but
// the behaviour is otherwise identical to the Unix re-exec.
func reExec(currentExe string) error {
	// #nosec G204 -- currentExe is os.Executable() resolved through
	// EvalSymlinks; same trust boundary as the running process.
	cmd := exec.Command(currentExe, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf(
			"start new binary after update: %w "+
				"(the upgrade succeeded — run `mk version` to confirm)", err)
	}

	// Wait so the user sees the new process's output inline rather
	// than the parent shell returning immediately. The new process
	// inherits stdin/stdout/stderr directly so output streams
	// correctly without us needing to plumb anything.
	if err := cmd.Wait(); err != nil {
		// Propagate the exit code from the new process so scripts
		// wrapping mk update don't think the upgrade failed when the
		// follow-up command did.
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("run new binary after update: %w", err)
	}
	return nil
}
