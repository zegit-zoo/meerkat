package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrHomebrewManaged is returned when an in-place binary swap is asked
// of an installation Homebrew owns.
//
// `mk update` replaces the running binary inside its install directory.
// For a Homebrew install that directory is the Cellar, and everything
// about it — the file, its mode, the symlinks brew wires into
// $HOMEBREW_PREFIX/bin, the receipt in INSTALL_RECEIPT.json — belongs
// to brew. Swapping the binary underneath it would leave brew
// reporting the old version, and the very next `brew upgrade` (or
// `brew reinstall`, or a dependent formula's relink) would silently
// throw the self-updated binary away. Refuse instead, and point at the
// package manager that actually owns the install.
//
// The wording stays tool-agnostic because both `mk update` and
// meerkat-bootstrap return it, and each already prefixes its own name
// when printing.
var ErrHomebrewManaged = errors.New(
	"this install is managed by Homebrew — replacing the binary in place would be undone by the next `brew upgrade`; run `brew update && brew upgrade meerkat` instead")

// HomebrewUpgradeCommand is the update path for a Homebrew install,
// used wherever a message would otherwise say `mk update`.
//
// It refreshes the tap first. The update nag reads the latest GitHub
// release, but brew only re-reads a tap on its own auto-update schedule
// (HOMEBREW_AUTO_UPDATE_SECS, a day by default), so a bare
// `brew upgrade meerkat` right after a release often answers "already up
// to date" (#70 review). The tap itself also trails a release by the
// hours its bump job takes.
const HomebrewUpgradeCommand = "brew update && brew upgrade meerkat"

// IsHomebrewInstall reports whether exe lives inside a Homebrew
// Cellar, i.e. whether the binary is owned by a `brew`-installed
// formula rather than by the user.
//
// exe is expected to be fully resolved already (os.Executable followed
// by filepath.EvalSymlinks — see RunningFromHomebrew): a Homebrew
// install is reached through a symlink in $HOMEBREW_PREFIX/bin, which
// is NOT itself under the Cellar, so checking an unresolved path would
// miss every real install.
//
// Two signals, either of which is sufficient:
//
//   - a path component named exactly "Cellar" — covers the three
//     standard prefixes (/opt/homebrew, /usr/local,
//     /home/linuxbrew/.linuxbrew) plus any relocated one, without
//     hardcoding a list;
//   - being under $HOMEBREW_PREFIX/Cellar when HOMEBREW_PREFIX is set
//     and non-empty.
//
// The function is pure apart from that one env lookup, so it is
// directly unit-testable with synthetic paths.
//
// Homebrew exists only on macOS and Linux; on Windows this is
// effectively always false, but the comparison still goes through
// filepath so a Windows-style path is split on the right separator
// rather than treated as one long component.
func IsHomebrewInstall(exe string) bool {
	if exe == "" {
		return false
	}
	clean := filepath.Clean(exe)

	// "Cellar" must be a whole path component: /opt/homebrew/Cellar/…
	// is a Homebrew install, but ~/.local/bin/Cellar-notes or
	// /srv/CellarKeeper/bin/meerkat are not.
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == "Cellar" {
			return true
		}
	}

	if prefix := strings.TrimSpace(os.Getenv("HOMEBREW_PREFIX")); prefix != "" {
		if isUnderDir(clean, filepath.Join(filepath.Clean(prefix), "Cellar")) {
			return true
		}
	}
	return false
}

// RunningFromHomebrew resolves the running executable the same way
// SwapAndReExec does — os.Executable, then EvalSymlinks, because the
// user invokes `mk`/`meerkat` through a symlink in
// $HOMEBREW_PREFIX/bin — and reports whether the result is a Homebrew
// install.
//
// Any error resolving the path answers "no": an undetectable install
// location must not block a self-update that would otherwise work.
func RunningFromHomebrew() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return false
	}
	return IsHomebrewInstall(resolved)
}

// isUnderDir reports whether path is dir itself or lives beneath it,
// comparing whole components so "/opt/homebrewXX" is not "under"
// "/opt/homebrew".
func isUnderDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
