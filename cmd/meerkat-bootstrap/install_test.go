package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/update"
)

// TestDecideProceed_DowngradeRefusedWithoutForce is the CLI-level
// "downgrade refusal + --force" acceptance criterion: without --force,
// a target strictly older than what's installed must be refused, with
// a message that names both versions and points at --force.
func TestDecideProceed_DowngradeRefusedWithoutForce(t *testing.T) {
	err := decideProceed("v0.8.0", "v0.8.6", false)
	if err == nil {
		t.Fatal("expected a downgrade to be refused")
	}
	for _, want := range []string{"v0.8.0", "v0.8.6", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got: %v", want, err)
		}
	}
}

// TestDecideProceed_ForceAllowsDowngrade: the same downgrade is
// allowed once --force is set.
func TestDecideProceed_ForceAllowsDowngrade(t *testing.T) {
	if err := decideProceed("v0.8.0", "v0.8.6", true); err != nil {
		t.Errorf("expected --force to allow a downgrade, got: %v", err)
	}
}

// TestDecideProceed_MigrationCaseNeedsNoForce is the whole point of
// this tool: a downstream fork sitting on an advanced v0.8.x series
// must be able to accept a numerically-newer upstream release with no
// --force needed, exactly like `mk update`'s own downgrade guard
// (internal/cli/update.go) already behaves for the same case. See
// docs/design/upstream-migration.md.
func TestDecideProceed_MigrationCaseNeedsNoForce(t *testing.T) {
	if err := decideProceed("v0.9.0", "v0.8.6", false); err != nil {
		t.Errorf("expected upstream v0.9.0 to be accepted over downstream v0.8.6 without --force, got: %v", err)
	}
}

// TestDecideProceed_SameVersionNeedsNoForce: re-running install
// against a destination already on the target version is a (harmless,
// useful for verification/repair) reinstall, not a downgrade.
func TestDecideProceed_SameVersionNeedsNoForce(t *testing.T) {
	if err := decideProceed("v0.10.0", "v0.10.0", false); err != nil {
		t.Errorf("expected a same-version reinstall to proceed without --force, got: %v", err)
	}
}

// TestResolveDestinationFlag_ExplicitValueWins: an explicit
// --destination is used as-is, with no $PATH lookup at all.
func TestResolveDestinationFlag_ExplicitValueWins(t *testing.T) {
	got, err := resolveDestinationFlag("/some/explicit/path")
	if err != nil {
		t.Fatalf("resolveDestinationFlag: %v", err)
	}
	if got != "/some/explicit/path" {
		t.Errorf("got %q, want the explicit value unchanged", got)
	}
}

// TestResolveDestinationFlag_FallsBackToPath confirms the documented
// default -- "the first of meerkat/mk found on $PATH" -- by putting a
// fixture `meerkat` executable on a scratch $PATH and leaving
// --destination empty.
func TestResolveDestinationFlag_FallsBackToPath(t *testing.T) {
	dir := t.TempDir()
	name := "meerkat"
	if runtime.GOOS == "windows" {
		name = "meerkat.exe"
	}
	fixture := filepath.Join(dir, name)
	if err := os.WriteFile(fixture, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := resolveDestinationFlag("")
	if err != nil {
		t.Fatalf("resolveDestinationFlag: %v", err)
	}
	resolvedFixture, err := filepath.EvalSymlinks(fixture)
	if err != nil {
		t.Fatal(err)
	}
	resolvedGot, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedGot != resolvedFixture {
		t.Errorf("got %q, want the fixture found on $PATH (%q)", got, fixture)
	}
}

// TestResolveDestinationFlag_NoDestinationAndNothingOnPathErrors: an
// empty --destination with neither `meerkat` nor `mk` resolvable must
// fail with a clear, actionable error rather than silently picking
// something unexpected.
func TestResolveDestinationFlag_NoDestinationAndNothingOnPathErrors(t *testing.T) {
	dir := t.TempDir() // deliberately empty -- nothing named meerkat/mk here
	t.Setenv("PATH", dir)

	_, err := resolveDestinationFlag("")
	if err == nil {
		t.Fatal("expected an error when --destination is empty and nothing is on $PATH")
	}
	if !strings.Contains(err.Error(), "--destination") {
		t.Errorf("expected error to mention --destination, got: %v", err)
	}
}

// TestRefuseHomebrewDestination_CellarPathRefused: meerkat-bootstrap
// installs over an existing binary in place, which is exactly what
// must not happen inside a Homebrew Cellar — brew owns that file and
// the next `brew upgrade` would discard the install. The destination
// is given as the $HOMEBREW_PREFIX/bin-style symlink, since that is
// what $PATH resolution (and so the default --destination) yields.
func TestRefuseHomebrewDestination_CellarPathRefused(t *testing.T) {
	prefix := t.TempDir()
	cellarBin := filepath.Join(prefix, "Cellar", "meerkat", "0.11.1", "bin")
	if err := os.MkdirAll(cellarBin, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(cellarBin, "meerkat")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	brewBin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(brewBin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(brewBin, "meerkat")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	err := refuseHomebrewDestination(link)
	if err == nil {
		t.Fatal("expected a Cellar destination to be refused")
	}
	if !errors.Is(err, update.ErrHomebrewManaged) {
		t.Errorf("expected update.ErrHomebrewManaged, got: %v", err)
	}
	if !strings.Contains(err.Error(), "brew upgrade meerkat") {
		t.Errorf("expected the brew upgrade hint, got: %v", err)
	}
}

// TestRefuseHomebrewDestination_NormalPathAllowed: an ordinary
// user-owned install is untouched by the guard.
func TestRefuseHomebrewDestination_NormalPathAllowed(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(dest, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := refuseHomebrewDestination(dest); err != nil {
		t.Errorf("expected a non-Cellar destination to pass, got: %v", err)
	}
}

// TestRefuseHomebrewDestination_UnresolvablePathIsNotThisGuardsProblem:
// a missing destination must fall through to the install flow's own
// (much better) error, not be swallowed or rewritten here.
func TestRefuseHomebrewDestination_UnresolvablePathIsNotThisGuardsProblem(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here", "meerkat")
	if err := refuseHomebrewDestination(missing); err != nil {
		t.Errorf("expected a missing destination to pass through, got: %v", err)
	}
}
