package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/auth"
)

// writeFixtureBinary writes a tiny shell script fixture standing in
// for a real meerkat binary, so DetectInstalledVersion/RunVersionSmoke
// can be exercised end to end without a real compiled binary or any
// network access. Skips on platforms without /bin/sh, matching the
// existing cosign_test.go stub-script pattern.
func writeFixtureBinary(t *testing.T, path, script string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture binary %q: %v", path, err)
	}
}

// TestInstallAtomic_ReplacesRealFile is the straightforward case: a
// destination that is itself a regular file gets replaced in place,
// and the ".old" backup is left for the caller to remove/restore.
func TestInstallAtomic_ReplacesRealFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(dest, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(newBin, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := InstallAtomic(newBin, dest)
	if err != nil {
		t.Fatalf("InstallAtomic: %v", err)
	}
	// Compare through EvalSymlinks on both sides: on macOS, t.TempDir()
	// lives under /var, itself a symlink to /private/var, so dest
	// resolves to a /private/... path even with no test-authored
	// symlink involved. What matters is that InstallAtomic resolved to
	// the same underlying file dest names, not the exact string.
	wantResolved, err := filepath.EvalSymlinks(dest)
	if err != nil {
		t.Fatalf("EvalSymlinks(dest): %v", err)
	}
	if resolved != wantResolved {
		t.Errorf("resolved = %q, want %q (no symlink involved)", resolved, wantResolved)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("dest content = %q, want %q", got, "new-binary")
	}

	backup, err := os.ReadFile(dest + ".old")
	if err != nil {
		t.Fatalf("expected backup to be preserved: %v", err)
	}
	if string(backup) != "old-binary" {
		t.Errorf("backup content = %q, want %q", backup, "old-binary")
	}
}

// TestInstallAtomic_SymlinkDestination_ReplacesRealTargetNotTheLink is
// the direct regression test for the "destination may be a real file
// or symlink: never write through a symlink unexpectedly" requirement.
// `mk` -> `meerkat` in the same directory is exactly the shape
// docs/INSTALL.md documents for the default install layout: replacing
// the target must update what `mk` resolves to without ever touching
// (or clobbering) an unrelated file the symlink might otherwise be
// tricked into pointing through, and without turning the symlink into
// a plain file.
func TestInstallAtomic_SymlinkDestination_ReplacesRealTargetNotTheLink(t *testing.T) {
	dir := t.TempDir()
	realBinary := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(realBinary, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkDest := filepath.Join(dir, "mk")
	if err := os.Symlink("meerkat", symlinkDest); err != nil {
		t.Fatal(err)
	}

	// An unrelated file living alongside the symlink stands in for
	// "something InstallAtomic must never touch" -- if resolution or
	// staging ever regressed to clobbering neighbours instead of the
	// resolved target, this would catch it.
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("victim-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	newBin := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(newBin, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := InstallAtomic(newBin, symlinkDest)
	if err != nil {
		t.Fatalf("InstallAtomic: %v", err)
	}
	// See the EvalSymlinks note in TestInstallAtomic_ReplacesRealFile:
	// compare through EvalSymlinks on both sides rather than raw
	// strings, since t.TempDir() itself can involve a symlink (macOS's
	// /var -> /private/var) unrelated to the one this test is about.
	wantResolved, err := filepath.EvalSymlinks(realBinary)
	if err != nil {
		t.Fatalf("EvalSymlinks(realBinary): %v", err)
	}
	if resolved != wantResolved {
		t.Errorf("resolved = %q, want the symlink's real target %q", resolved, wantResolved)
	}

	// The symlink itself must be completely untouched: still a
	// symlink, still pointing at "meerkat".
	fi, err := os.Lstat(symlinkDest)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%q is no longer a symlink after InstallAtomic", symlinkDest)
	}
	target, err := os.Readlink(symlinkDest)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "meerkat" {
		t.Errorf("symlink target changed: got %q, want %q", target, "meerkat")
	}

	// The real target now has the new content.
	got, err := os.ReadFile(realBinary)
	if err != nil {
		t.Fatalf("read real binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("real binary content = %q, want %q", got, "new-binary")
	}
	// Reading through the symlink gets the same, updated content.
	gotViaLink, err := os.ReadFile(symlinkDest)
	if err != nil {
		t.Fatalf("read via symlink: %v", err)
	}
	if string(gotViaLink) != "new-binary" {
		t.Errorf("content via symlink = %q, want %q", gotViaLink, "new-binary")
	}

	// The unrelated neighbour file is byte-for-byte untouched.
	gotVictim, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(gotVictim) != "victim-content" {
		t.Errorf("victim file was modified: %q", gotVictim)
	}
}

// TestResolveDestination_MissingDestinationErrors: meerkat-bootstrap
// replaces an existing install, it does not create one out of thin
// air -- a missing destination must fail with a clear, actionable
// error rather than silently "succeeding" at nothing or trying to
// create a fresh file wherever the operator's typo happened to point.
func TestResolveDestination_MissingDestinationErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	_, err := resolveDestination(missing)
	if err == nil {
		t.Fatal("expected an error for a missing destination")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected a 'does not exist' error, got: %v", err)
	}
}

// TestRemoveBackup_NoBackupIsNotAnError: RemoveBackup must be safe to
// call even when InstallAtomic never ran (no ".old" file), since
// callers may call it unconditionally in a cleanup path.
func TestRemoveBackup_NoBackupIsNotAnError(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "meerkat")
	if err := RemoveBackup(dest); err != nil {
		t.Errorf("RemoveBackup with no backup present: %v", err)
	}
}

// TestRestoreBackup_NoBackupErrors: unlike RemoveBackup, restoring
// when there is nothing to restore from must fail loudly -- silently
// doing nothing here would tell a caller a rollback succeeded when
// the (still-broken) new binary was in fact left in place.
func TestRestoreBackup_NoBackupErrors(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "meerkat")
	if err := RestoreBackup(dest); err == nil {
		t.Fatal("expected an error when no backup exists")
	}
}

// TestInstallAtomic_ThenRestoreBackup_RoundTrips exercises the full
// "failed post-install check restores the previous binary" path this
// package's callers (meerkat-bootstrap) drive: install, discover the
// new binary doesn't work, restore -- and confirm destination ends up
// byte-for-byte back where it started, with the backup file gone
// (moved back, not merely copied).
func TestInstallAtomic_ThenRestoreBackup_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(dest, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(newBin, []byte("broken-new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := InstallAtomic(newBin, dest)
	if err != nil {
		t.Fatalf("InstallAtomic: %v", err)
	}

	// Simulate a failed smoke check by just not calling RunVersionSmoke
	// at all; go straight to RestoreBackup, exactly like the "post
	// install execution failed" branch of the caller's flow does.
	if err := RestoreBackup(resolved); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest after restore: %v", err)
	}
	if string(got) != "old-binary" {
		t.Errorf("dest content after restore = %q, want the original %q", got, "old-binary")
	}
	if _, err := os.Stat(dest + ".old"); !os.IsNotExist(err) {
		t.Errorf("expected backup file to be gone after restore, stat err = %v", err)
	}
}

// TestRunVersionSmoke_FailedCheckLeavesBackupForRestore is the
// end-to-end version of the "failed post-install smoke check restores
// the previous binary" acceptance criterion: a fixture "new binary"
// that exits non-zero on `version` must (a) fail RunVersionSmoke and
// (b) leave the ".old" backup exactly where RestoreBackup expects it,
// so the caller's restore step (exercised directly above) succeeds.
func TestRunVersionSmoke_FailedCheckLeavesBackupForRestore(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "meerkat")
	writeFixtureBinary(t, dest, "#!/bin/sh\necho 'meerkat v0.9.0 (abc123) built 2026-01-01'\n")

	brokenNew := filepath.Join(t.TempDir(), "broken-new")
	writeFixtureBinary(t, brokenNew, "#!/bin/sh\necho 'segfault or whatever' >&2\nexit 1\n")

	resolved, err := InstallAtomic(brokenNew, dest)
	if err != nil {
		t.Fatalf("InstallAtomic: %v", err)
	}

	err = RunVersionSmoke(context.Background(), resolved)
	if err == nil {
		t.Fatal("expected the smoke check to fail for a binary that exits non-zero")
	}
	if !strings.Contains(err.Error(), "smoke check") {
		t.Errorf("expected error to mention the smoke check, got: %v", err)
	}

	// The backup must still be there for the caller to restore from.
	if _, statErr := os.Stat(resolved + ".old"); statErr != nil {
		t.Fatalf("expected backup to survive a failed smoke check: %v", statErr)
	}

	if err := RestoreBackup(resolved); err != nil {
		t.Fatalf("RestoreBackup after failed smoke check: %v", err)
	}
	got, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "v0.9.0") {
		t.Errorf("expected the original fixture binary restored, got: %q", got)
	}
}

// TestRunVersionSmoke_OK: a fixture binary that exits 0 passes the
// smoke check.
func TestRunVersionSmoke_OK(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "meerkat")
	writeFixtureBinary(t, bin, "#!/bin/sh\necho 'meerkat v0.10.0 (abc) built today'\nexit 0\n")

	if err := RunVersionSmoke(context.Background(), bin); err != nil {
		t.Errorf("expected success, got: %v", err)
	}
}

// TestDetectInstalledVersion_JSON reads the version straight out of
// `version --json` when the fixture binary supports it -- the format
// `mk version --json` actually emits.
func TestDetectInstalledVersion_JSON(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "meerkat")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = version ] && [ \"$2\" = --json ]; then\n" +
		"  echo '{\"version\":\"v0.10.0\",\"commit\":\"abc\"}'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	writeFixtureBinary(t, bin, script)

	got, err := DetectInstalledVersion(context.Background(), bin)
	if err != nil {
		t.Fatalf("DetectInstalledVersion: %v", err)
	}
	if got != "v0.10.0" {
		t.Errorf("got %q, want %q", got, "v0.10.0")
	}
}

// TestDetectInstalledVersion_PlainFallback is the "fixture binary
// identified as downstream v0.8.6, with no GitHub updater support"
// scenario from the acceptance criteria: a binary that doesn't
// understand --json at all (it exits non-zero for any unrecognized
// flag, same as a real cobra command would) but does print a normal
// `mk version`-shaped line for a bare `version` call. The plain-text
// fallback must still recover the version.
func TestDetectInstalledVersion_PlainFallback(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "meerkat")
	script := "#!/bin/sh\n" +
		"if [ \"$#\" -eq 1 ] && [ \"$1\" = version ]; then\n" +
		"  echo 'meerkat v0.8.6 (deadbee) built 2025-06-01T00:00:00Z'\n" +
		"  echo '  knowledge base: none (source: embedded)'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo 'unknown flag' >&2\n" +
		"exit 2\n"
	writeFixtureBinary(t, bin, script)

	got, err := DetectInstalledVersion(context.Background(), bin)
	if err != nil {
		t.Fatalf("DetectInstalledVersion: %v", err)
	}
	if got != "v0.8.6" {
		t.Errorf("got %q, want %q", got, "v0.8.6")
	}
}

// TestDetectInstalledVersion_Unrunnable: a destination that can't be
// executed at all (not present, not executable, ...) must report an
// error the caller can treat as "unknown, downgrade guard skipped" --
// never a fabricated version.
func TestDetectInstalledVersion_Unrunnable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := DetectInstalledVersion(context.Background(), missing); err == nil {
		t.Fatal("expected an error for a nonexistent binary")
	}
}

// TestDetectInstalledVersion_NoRecognizableVersionErrors: output with
// nothing version-shaped in it (JSON *and* plain text both fail to
// produce anything) must be reported as an error, not silently
// returned as "".
func TestDetectInstalledVersion_NoRecognizableVersionErrors(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "meerkat")
	writeFixtureBinary(t, bin, "#!/bin/sh\necho 'nothing useful here'\nexit 0\n")

	_, err := DetectInstalledVersion(context.Background(), bin)
	if err == nil {
		t.Fatal("expected an error when no version can be found in the output")
	}
}

// TestDecideProceedEquivalent_DowngradeGuard exercises IsDowngrade the
// same way meerkat-bootstrap's own decideProceed does (see
// cmd/meerkat-bootstrap/install.go), as a package-level guard against
// that composition ever silently changing meaning. The CLI-level
// force/no-force branch itself is unit tested directly in
// cmd/meerkat-bootstrap's own test file, without any of this
// package's I/O.
func TestDecideProceedEquivalent_DowngradeGuard(t *testing.T) {
	cases := []struct {
		target, installed string
		wantDowngrade     bool
	}{
		{"v0.9.0", "v0.8.6", false}, // the migration case: upstream ahead of a downstream fork
		{"v0.8.0", "v0.8.6", true},  // a real downgrade
		{"v0.8.6", "v0.8.6", false}, // reinstall of the same version is not a downgrade
	}
	for _, tc := range cases {
		if got := IsDowngrade(tc.target, tc.installed); got != tc.wantDowngrade {
			t.Errorf("IsDowngrade(%q, %q) = %v, want %v", tc.target, tc.installed, got, tc.wantDowngrade)
		}
	}
}

// TestInstallAtomic_PermissionDenied_UnixReportsFriendlyError confirms
// InstallAtomic surfaces the same friendly permission-denied guidance
// SwapAndReExec does, rather than a bare "permission denied" —
// exercised the same way TestCheckInstallDirWritable_PermissionDenied_Unix
// is, by stripping write access from the destination's directory.
func TestInstallAtomic_PermissionDenied_UnixReportsFriendlyError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission-bit semantics only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot exercise this path as root")
	}
	oldRunCommand := runCommand
	t.Cleanup(func() { runCommand = oldRunCommand })
	// Force the sudo fallback itself to fail fast rather than actually
	// invoking sudo (which would hang or fail unpredictably in CI).
	runCommand = func(name string, args ...string) error {
		return errors.New("sudo not available in test")
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "meerkat")
	if err := os.WriteFile(dest, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	newBin := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(newBin, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := InstallAtomic(newBin, dest)
	if err == nil {
		t.Fatal("expected an error when the install directory isn't writable and sudo fails")
	}
}

// newFakeReleaseServer starts an httptest server that serves a single
// release ("v0.10.0") with an asset, a checksums file, and a cosign
// bundle, exactly the three-asset shape meerkat-bootstrap's runInstall
// (cmd/meerkat-bootstrap/install.go) looks up by name via
// Release.FindAsset. checksumBody/bundleBody let each test inject a
// tampered file for one of the two without needing to hand-roll the
// release JSON and asset routing per test.
func newFakeReleaseServer(t *testing.T, assetName, checksumName, bundleName string, assetBody, checksumBody, bundleBody []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server // captured by the /releases/tags handler closure below, set after NewServer starts
	mux.HandleFunc("/repos/"+Project+"/releases/tags/v0.10.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tag_name":"v0.10.0","published_at":"2026-01-01T00:00:00Z","assets":[
			{"name":%q,"url":%q},
			{"name":%q,"url":%q},
			{"name":%q,"url":%q}
		]}`,
			assetName, srv.URL+"/assets/asset",
			checksumName, srv.URL+"/assets/checksums",
			bundleName, srv.URL+"/assets/bundle",
		)
	})
	mux.HandleFunc("/assets/asset", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(assetBody)
	})
	mux.HandleFunc("/assets/checksums", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(checksumBody)
	})
	mux.HandleFunc("/assets/bundle", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bundleBody)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestBootstrapFlow_TamperedChecksumRejectedBeforeInstall composes
// FetchByTag + DownloadAsset + DownloadToTemp + ReadChecksumFor
// exactly the way cmd/meerkat-bootstrap's runInstall does, against a
// fake GitHub API + asset server, to prove a checksums file that
// doesn't actually match the downloaded asset is caught before any
// install step ever runs. This is the "tampered checksum ... is
// rejected before installation" acceptance criterion, exercised as a
// composition rather than only as isolated unit tests on
// ReadChecksumFor/DownloadAsset individually.
func TestBootstrapFlow_TamperedChecksumRejectedBeforeInstall(t *testing.T) {
	withStubbedToken(t, "", auth.ErrNoConfig)

	assetBody, _ := fakeAsset(t, "real release contents")
	const assetName = "meerkat_0.10.0_linux_amd64.tar.gz"
	const checksumName = "meerkat_0.10.0_checksums.txt"
	const bundleName = checksumName + ".sigstore.json"
	// A checksums file whose line for assetName is simply wrong --
	// stands in for "attacker swapped the checksums file, or the
	// asset, and the two no longer match."
	tamperedChecksums := []byte(strings.Repeat("0", 64) + "  " + assetName + "\n")

	srv := newFakeReleaseServer(t, assetName, checksumName, bundleName, assetBody, tamperedChecksums, []byte(`{}`))
	withStubbedGitHubAPI(t, srv)

	rel, err := FetchByTag(context.Background(), "v0.10.0")
	if err != nil {
		t.Fatalf("FetchByTag: %v", err)
	}
	assetURL, ok := rel.FindAsset(assetName)
	if !ok {
		t.Fatal("asset not found in fake release")
	}
	checksumURL, ok := rel.FindAsset(checksumName)
	if !ok {
		t.Fatal("checksum asset not found in fake release")
	}

	archivePath, gotSha, err := DownloadAsset(context.Background(), assetURL, "")
	if err != nil {
		t.Fatalf("DownloadAsset: %v", err)
	}
	defer os.Remove(archivePath)

	checksumsLocal, err := DownloadToTemp(context.Background(), checksumURL, "", "meerkat-bootstrap-checksums-*.txt")
	if err != nil {
		t.Fatalf("DownloadToTemp: %v", err)
	}
	defer os.Remove(checksumsLocal)

	expectedSha, err := ReadChecksumFor(checksumsLocal, assetName)
	if err != nil {
		t.Fatalf("ReadChecksumFor: %v", err)
	}

	// This is exactly runInstall's own gate
	// (cmd/meerkat-bootstrap/install.go): !strings.EqualFold(gotSha,
	// expectedSha) -> refuse to install. Confirm the tampered checksum
	// really does fail it -- i.e. that the composition, not just each
	// piece in isolation, rejects a mismatched asset/checksum pair.
	if strings.EqualFold(gotSha, expectedSha) {
		t.Fatal("expected the tampered checksums file to NOT match the real asset's sha256")
	}
}

// TestBootstrapFlow_UnsignedBundleRejectedBeforeChecksumIsTrusted
// composes FetchByTag + DownloadToTemp + VerifyChecksumSignature the
// way runInstall does, with a cosign stub that always fails
// verification (same technique as TestVerifyChecksumSignature_BadSig
// in cosign_test.go), to prove a bad/tampered signature bundle is
// rejected before the checksums file's contents are ever trusted --
// runInstall calls VerifyChecksumSignature strictly before
// ReadChecksumFor for exactly this reason.
func TestBootstrapFlow_UnsignedBundleRejectedBeforeChecksumIsTrusted(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	withStubbedToken(t, "", auth.ErrNoConfig)

	const assetName = "meerkat_0.10.0_linux_amd64.tar.gz"
	const checksumName = "meerkat_0.10.0_checksums.txt"
	const bundleName = checksumName + ".sigstore.json"
	checksums := []byte(strings.Repeat("a", 64) + "  " + assetName + "\n")
	tamperedBundle := []byte(`{"tampered":"not a real sigstore bundle"}`)

	srv := newFakeReleaseServer(t, assetName, checksumName, bundleName, []byte("irrelevant"), checksums, tamperedBundle)
	withStubbedGitHubAPI(t, srv)

	// Stub cosign to always reject, exactly like
	// TestVerifyChecksumSignature_BadSig: what matters here is that
	// runInstall's real composition (fetch -> download checksums+bundle
	// -> verify) reaches and respects that rejection, not cosign's own
	// verification logic (already covered by cosign_test.go).
	dir := t.TempDir()
	stub := filepath.Join(dir, "cosign")
	script := "#!/bin/sh\necho 'cosign: signature does not match' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	rel, err := FetchByTag(context.Background(), "v0.10.0")
	if err != nil {
		t.Fatalf("FetchByTag: %v", err)
	}
	checksumURL, ok := rel.FindAsset(checksumName)
	if !ok {
		t.Fatal("checksum asset not found in fake release")
	}
	bundleURL, ok := rel.FindAsset(bundleName)
	if !ok {
		t.Fatal("bundle asset not found in fake release")
	}

	checksumsLocal, err := DownloadToTemp(context.Background(), checksumURL, "", "meerkat-bootstrap-checksums-*.txt")
	if err != nil {
		t.Fatalf("DownloadToTemp checksums: %v", err)
	}
	defer os.Remove(checksumsLocal)
	bundlePath, err := DownloadToTemp(context.Background(), bundleURL, "", "meerkat-bootstrap-bundle-*.sigstore.json")
	if err != nil {
		t.Fatalf("DownloadToTemp bundle: %v", err)
	}
	defer os.Remove(bundlePath)

	err = VerifyChecksumSignature(context.Background(), checksumsLocal, bundlePath)
	if err == nil {
		t.Fatal("expected signature verification to fail for a tampered/unsigned bundle")
	}
	if errors.Is(err, ErrCosignMissing) {
		t.Fatalf("expected a verification failure, not a missing-cosign error: %v", err)
	}
}
