package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

// InstallAtomic stages newPath and atomically swaps it into
// destination, reusing exactly the same staging/backup/promote
// primitives (and, on Unix, the same sudo elevation fallback) as
// SwapAndReExec's own install step. It differs from SwapAndReExec in
// two ways that matter for a bootstrap tool rather than a running
// binary updating itself:
//
//   - destination is resolved through any symlink first (see
//     resolveDestination), so a symlinked destination (e.g. `mk` ->
//     `meerkat` in the same directory) has its real target replaced
//     rather than the symlink entry itself being clobbered.
//   - It never re-execs, and it never deletes the "<destination>.old"
//     backup on its own — even on Unix, where installStaged normally
//     removes it immediately. The caller is expected to run its own
//     post-install verification (e.g. RunVersionSmoke) and then call
//     RemoveBackup on success or RestoreBackup on failure.
//
// Returns the resolved (symlink-followed) destination path that was
// actually written, so the caller's smoke check and backup cleanup
// operate on the same path this function did.
func InstallAtomic(newPath, destination string) (resolved string, err error) {
	resolved, err = resolveDestination(destination)
	if err != nil {
		return "", err
	}

	stagedPath, cleanup, err := stageInTemp(newPath)
	if err != nil {
		return "", err
	}
	defer cleanup()

	if err := swapWithBackup(stagedPath, resolved); err != nil {
		if !isPermission(err) {
			return "", err
		}
		if runtime.GOOS == "windows" {
			// No sudo on Windows; surface the friendly elevation
			// message and stop, exactly like SwapAndReExec does.
			return "", permissionDeniedError(resolved, "install", err)
		}
		fmt.Fprintf(os.Stderr, "meerkat-bootstrap: install directory requires elevated privileges; running sudo for the final copy/move only.\n")
		if _, sudoErr := swapWithBackupSudo(stagedPath, resolved); sudoErr != nil {
			return "", wrapSudoInstallError(resolved, sudoErr)
		}
	}
	return resolved, nil
}

// resolveDestination follows any symlink at destination to the real
// file it points at, so InstallAtomic replaces the intended install
// entry rather than the symlink itself — mirroring SwapAndReExec's
// treatment of os.Executable(). Unlike SwapAndReExec's currentExe
// (which always exists, being the running process's own binary),
// destination is an arbitrary operator-supplied path, so a missing
// destination is reported as a clear error rather than silently
// treated as "nothing to resolve": meerkat-bootstrap replaces an
// existing install in place, it does not create a fresh one.
func resolveDestination(destination string) (string, error) {
	abs, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve destination path: %w", err)
	}
	if _, err := os.Lstat(abs); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("destination %q does not exist — meerkat-bootstrap replaces an existing binary in place; pass --destination pointing at the installed meerkat/mk binary you want to replace", destination)
		}
		return "", fmt.Errorf("stat destination %q: %w", destination, err)
	}
	resolvedPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve destination symlinks: %w", err)
	}
	return resolvedPath, nil
}

// RemoveBackup deletes the "<destination>.old" backup file InstallAtomic
// leaves behind, once the caller's post-install verification (e.g. a
// `version` smoke check) has confirmed the newly installed binary
// works. Safe to call even if no backup exists.
func RemoveBackup(destination string) error {
	err := os.Remove(destination + ".old")
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove backup: %w", err)
	}
	return nil
}

// RestoreBackup reverts destination to the "<destination>.old" backup
// InstallAtomic leaves behind — used when a post-install check (the
// new binary's own `version` smoke check, most notably) fails after
// the swap has already happened. destination must be the resolved
// path InstallAtomic returned, not the possibly-symlinked path the
// operator originally passed in.
func RestoreBackup(destination string) error {
	backupPath := destination + ".old"
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("no backup found at %q to restore: %w", backupPath, err)
	}
	if err := os.Rename(backupPath, destination); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}
	return nil
}

// versionSmokeTimeout bounds how long RunVersionSmoke and
// DetectInstalledVersion wait for a `version` subprocess before giving
// up. Generous relative to how fast this command should realistically
// return, but short enough that a hung or misbehaving binary (e.g. one
// that blocks on stdin) doesn't wedge the bootstrap indefinitely.
const versionSmokeTimeout = 15 * time.Second

// RunVersionSmoke runs "<binaryPath> version" and returns nil only if
// it exits 0. This is the post-install check meerkat-bootstrap uses to
// decide whether to keep the freshly installed binary (RemoveBackup)
// or restore the previous one (RestoreBackup): a binary that was
// downloaded, sha256- and cosign-verified, but simply doesn't run on
// this machine (wrong architecture packaged by mistake, a corrupted
// extraction that still happened to hash-match, missing shared
// libraries, ...) must not be left in place.
func RunVersionSmoke(ctx context.Context, binaryPath string) error {
	c, cancel := context.WithTimeout(ctx, versionSmokeTimeout)
	defer cancel()
	// #nosec G204 -- binaryPath is the file this process itself just
	// verified (sha256 + cosign) and installed; not user-controlled
	// argv.
	cmd := exec.CommandContext(c, binaryPath, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("smoke check `%s version` failed: %w\n%s", binaryPath, err, bytes.TrimSpace(out))
	}
	return nil
}

// installedVersionJSON is the subset of `<binary> version --json`'s
// output DetectInstalledVersion needs. internal/cli's versionInfo has
// many more fields; only Version matters here.
type installedVersionJSON struct {
	Version string `json:"version"`
}

// versionTokenPattern matches a SemVer-shaped token (with or without a
// leading "v"), used to pull a version out of free-form `version`
// output for binaries that predate (or never added) a --json flag.
// Deliberately loose about the pre-release/build-metadata tail —
// parseSemver in semver.go is the strict parser; this only needs to
// locate the candidate substring to hand it.
var versionTokenPattern = regexp.MustCompile(`v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

// DetectInstalledVersion runs "<binaryPath> version" and returns the
// installed binary's own reported version, so meerkat-bootstrap's
// downgrade guard (IsDowngrade) can compare against whatever is
// actually at the destination — not a value baked into the bootstrap
// binary itself.
//
// It tries "version --json" first (the format `mk version --json`
// emits: {"version": "vX.Y.Z", ...}) and falls back to parsing the
// first SemVer-shaped token out of plain "version" output, so a
// downstream fork's binary built before --json existed (or one that
// never added it) is still readable — the only documented
// requirement is that the destination binary understands a bare
// `version` subcommand, which every fork descended from this codebase
// does.
//
// Returns ("", err) if the binary can't be run at all, or if neither
// form of its output contains anything that looks like a version.
// Callers must treat that as "unknown, can't tell" — exactly like
// IsDowngrade's own handling of an unparseable version — never as
// license to either block or skip the downgrade guard by assumption.
func DetectInstalledVersion(ctx context.Context, binaryPath string) (string, error) {
	if v, err := detectInstalledVersionJSON(ctx, binaryPath); err == nil {
		return v, nil
	}
	return detectInstalledVersionPlain(ctx, binaryPath)
}

func detectInstalledVersionJSON(ctx context.Context, binaryPath string) (string, error) {
	c, cancel := context.WithTimeout(ctx, versionSmokeTimeout)
	defer cancel()
	// #nosec G204 -- binaryPath is the operator-supplied --destination
	// this whole command exists to inspect/replace, not arbitrary
	// input threaded through from a network response.
	cmd := exec.CommandContext(c, binaryPath, "version", "--json")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run `%s version --json`: %w", binaryPath, err)
	}
	var v installedVersionJSON
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("parse `%s version --json` output: %w", binaryPath, err)
	}
	if v.Version == "" {
		return "", fmt.Errorf("`%s version --json` reported an empty version", binaryPath)
	}
	return v.Version, nil
}

func detectInstalledVersionPlain(ctx context.Context, binaryPath string) (string, error) {
	c, cancel := context.WithTimeout(ctx, versionSmokeTimeout)
	defer cancel()
	// #nosec G204 -- see detectInstalledVersionJSON.
	cmd := exec.CommandContext(c, binaryPath, "version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run `%s version`: %w", binaryPath, err)
	}
	tok := versionTokenPattern.Find(out)
	if tok == nil {
		return "", fmt.Errorf("could not find a version in `%s version` output: %q", binaryPath, bytes.TrimSpace(out))
	}
	return string(tok), nil
}
