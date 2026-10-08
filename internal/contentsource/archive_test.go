package contentsource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tarEntry is a small builder for one archive/tar entry, used by
// buildTarGz below to construct both well-formed and adversarial test
// archives without repeating tar/gzip boilerplate at every call site.
type tarEntry struct {
	name     string
	typeflag byte
	linkname string
	content  string
	size     int64 // overrides len(content) when non-zero (declaring a size larger than what's actually written)
}

func regEntry(name, content string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, content: content}
}

func dirEntry(name string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeDir}
}

func symlinkEntry(name, target string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeSymlink, linkname: target}
}

func hardlinkEntry(name, target string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeLink, linkname: target}
}

// buildTarGz renders entries into a gzip-compressed tar archive.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := int64(0o644)
		if e.typeflag == tar.TypeDir {
			mode = 0o755
		}
		size := int64(len(e.content))
		if e.size != 0 {
			size = e.size
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     mode,
			Size:     size,
		}
		if e.typeflag == tar.TypeSymlink || e.typeflag == tar.TypeLink || e.typeflag == tar.TypeDir {
			hdr.Size = 0 // tar requires zero size for links/dirs
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.name, err)
		}
		if e.content != "" && hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

// writeTarGzFile writes body to a fresh tempfile inside dir and returns
// its path.
func writeTarGzFile(t *testing.T, dir string, body []byte) string {
	t.Helper()
	p := filepath.Join(dir, "archive.tar.gz")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// --- functional ---

func TestExtractTarGz_Basic(t *testing.T) {
	body := buildTarGz(t, []tarEntry{
		dirEntry("wiki"),
		regEntry("wiki/index.md", "# Index\n"),
		regEntry("wiki/concepts/widgets.md", "# Widgets\n"),
		regEntry("ingestion/sources.yaml", "sources: []\n"),
	})
	src := writeTarGzFile(t, t.TempDir(), body)
	dest := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(src, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "wiki", "index.md"))
	if err != nil {
		t.Fatalf("read wiki/index.md: %v", err)
	}
	if string(got) != "# Index\n" {
		t.Errorf("wiki/index.md = %q", got)
	}
	// A nested file whose parent dir ("wiki/concepts") was never given
	// its own explicit tar.TypeDir entry must still be created (via
	// MkdirAll inside extractRegularFile).
	got, err = os.ReadFile(filepath.Join(dest, "wiki", "concepts", "widgets.md"))
	if err != nil {
		t.Fatalf("read wiki/concepts/widgets.md (implicit parent dir): %v", err)
	}
	if string(got) != "# Widgets\n" {
		t.Errorf("wiki/concepts/widgets.md = %q", got)
	}
	if _, err := os.ReadFile(filepath.Join(dest, "ingestion", "sources.yaml")); err != nil {
		t.Fatalf("read ingestion/sources.yaml: %v", err)
	}
}

// TestExtractTarGz_PermissionsNormalised guards against trusting the
// archive's own mode bits: an entry declaring an unusual/dangerous mode
// (e.g. world-writable, or setuid-like bits) must still land as the
// fixed 0644 (files, subject to umask) this package always uses.
func TestExtractTarGz_PermissionsNormalised(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := "x"
	if err := tw.WriteHeader(&tar.Header{Name: "weird.md", Typeflag: tar.TypeReg, Mode: 0o777, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	src := writeTarGzFile(t, t.TempDir(), buf.Bytes())
	dest := t.TempDir()
	if err := extractTarGz(src, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dest, "weird.md"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("weird.md perm = %o, want 0644 regardless of the archive's declared 0777", perm)
	}
}

// --- hardening: the primary escape vector (symlinks / hardlinks) ---

// TestExtractTarGz_SymlinkSkipped_AbsoluteTarget is proof (a) from the
// hardening checklist: a symlink entry pointing at an absolute path
// outside the destination must be skipped outright — never created, so
// it can never later resolve outside the tree either.
func TestExtractTarGz_SymlinkSkipped_AbsoluteTarget(t *testing.T) {
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "passwd")
	if err := os.WriteFile(secretFile, []byte("SECRET-ABS-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}

	body := buildTarGz(t, []tarEntry{
		regEntry("wiki/index.md", "home\n"),
		symlinkEntry("wiki/leak.md", secretFile),
	})
	src := writeTarGzFile(t, t.TempDir(), body)
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Logf("malicious entry: symlink wiki/leak.md -> %s (absolute, outside dest %s)", secretFile, dest)
	if err := extractTarGz(src, dest); err != nil {
		t.Fatalf("extractTarGz should skip the symlink, not fail the whole archive: %v", err)
	}

	// The legitimate entry still extracts...
	if _, err := os.ReadFile(filepath.Join(dest, "wiki", "index.md")); err != nil {
		t.Errorf("wiki/index.md missing after extraction: %v", err)
	}
	t.Log("legitimate entry wiki/index.md: extracted OK")
	// ...but the symlink entry itself must not exist at all.
	_, lstatErr := os.Lstat(filepath.Join(dest, "wiki", "leak.md"))
	if !errors.Is(lstatErr, os.ErrNotExist) {
		t.Fatalf("wiki/leak.md exists after extraction (Lstat err=%v) — symlink entry was not skipped", lstatErr)
	}
	t.Logf("wiki/leak.md: Lstat = %v (never created — symlink entry skipped, not followed)", lstatErr)
	assertNoSecretBytesAnywhere(t, destParent, "SECRET-ABS-BYTES")
	t.Logf("confirmed: SECRET-ABS-BYTES does not appear anywhere under %s", destParent)
}

// TestExtractTarGz_SymlinkSkipped_RelativeEscapingTarget covers a
// relative Linkname that walks upward out of the destination
// (`../../../../etc/passwd`-shaped) — same defense, different target
// shape, since a hostile archive isn't required to use an absolute path
// to escape via a symlink.
func TestExtractTarGz_SymlinkSkipped_RelativeEscapingTarget(t *testing.T) {
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "passwd")
	if err := os.WriteFile(secretFile, []byte("SECRET-REL-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(dest, "wiki"), secretFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("test setup bug: %q does not escape via .. as intended", rel)
	}

	body := buildTarGz(t, []tarEntry{
		symlinkEntry("wiki/leak-rel.md", rel),
	})
	src := writeTarGzFile(t, t.TempDir(), body)

	if err := extractTarGz(src, dest); err != nil {
		t.Fatalf("extractTarGz should skip the symlink, not fail: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "wiki", "leak-rel.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wiki/leak-rel.md exists after extraction (Lstat err=%v)", err)
	}
	assertNoSecretBytesAnywhere(t, destParent, "SECRET-REL-BYTES")
}

// TestExtractTarGz_HardlinkSkipped: hardlink (tar.TypeLink) entries get
// the identical treatment as symlinks — never created, never followed.
func TestExtractTarGz_HardlinkSkipped(t *testing.T) {
	body := buildTarGz(t, []tarEntry{
		regEntry("wiki/index.md", "home\n"),
		hardlinkEntry("wiki/hardlink.md", "/etc/passwd"),
	})
	src := writeTarGzFile(t, t.TempDir(), body)
	dest := t.TempDir()

	if err := extractTarGz(src, dest); err != nil {
		t.Fatalf("extractTarGz should skip the hardlink, not fail: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "wiki", "hardlink.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wiki/hardlink.md exists after extraction (Lstat err=%v)", err)
	}
}

// --- hardening: path-traversal / absolute-path entry names ---

// TestExtractTarGz_DotDotEntryRejected is proof (b): a `../escape.md`
// entry must abort the whole extraction (not merely be silently
// skipped) and must never write anything outside the destination.
func TestExtractTarGz_DotDotEntryRejected(t *testing.T) {
	body := buildTarGz(t, []tarEntry{
		regEntry("../escape.md", "PWNED"),
	})
	src := writeTarGzFile(t, t.TempDir(), body)
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Logf("malicious entry: regular file \"../escape.md\" (dest=%s)", dest)
	err := extractTarGz(src, dest)
	if err == nil {
		t.Fatal("expected an error for a '../escape.md' entry, got nil")
	}
	t.Logf("extractTarGz returned an error (whole extraction refused): %v", err)
	if !strings.Contains(err.Error(), "unsafe name") {
		t.Errorf("error = %v, want it to mention the unsafe name", err)
	}
	// "../escape.md" resolved against dest would land exactly one level
	// up, i.e. directly inside destParent — the precise location a
	// successful traversal would have written to.
	escapePath := filepath.Join(destParent, "escape.md")
	if _, statErr := os.Stat(escapePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("escape.md was written into the parent of dest — traversal succeeded")
	}
	t.Logf("confirmed: %s does not exist (nothing written outside dest)", escapePath)
	assertNoSecretBytesAnywhere(t, destParent, "PWNED")
}

// TestExtractTarGz_DeepDotDotEntryRejected covers a multi-level escape
// (`../../etc/passwd`-shaped) — path.Clean does not eliminate a leading
// ".." run, so this must be caught the same way as the single-level case.
func TestExtractTarGz_DeepDotDotEntryRejected(t *testing.T) {
	body := buildTarGz(t, []tarEntry{
		regEntry("../../../../tmp/escape.md", "PWNED"),
	})
	src := writeTarGzFile(t, t.TempDir(), body)
	dest := t.TempDir()

	if err := extractTarGz(src, dest); err == nil {
		t.Fatal("expected an error for a deeply-escaping entry, got nil")
	}
}

// TestExtractTarGz_DotDotMidPathRejected: the traversal doesn't have to
// be at the front — "wiki/../../escape.md" cleans to an upward escape
// too and must be rejected the same way.
func TestExtractTarGz_DotDotMidPathRejected(t *testing.T) {
	body := buildTarGz(t, []tarEntry{
		regEntry("wiki/../../escape.md", "PWNED"),
	})
	src := writeTarGzFile(t, t.TempDir(), body)
	dest := t.TempDir()

	if err := extractTarGz(src, dest); err == nil {
		t.Fatal("expected an error for 'wiki/../../escape.md', got nil")
	}
}

// TestExtractTarGz_AbsolutePathEntryRejected is proof (c): an
// absolute-path entry must be rejected outright.
func TestExtractTarGz_AbsolutePathEntryRejected(t *testing.T) {
	body := buildTarGz(t, []tarEntry{
		regEntry("/etc/passwd-clone", "PWNED"),
	})
	src := writeTarGzFile(t, t.TempDir(), body)
	dest := t.TempDir()

	t.Log("malicious entry: regular file \"/etc/passwd-clone\" (absolute path)")
	err := extractTarGz(src, dest)
	if err == nil {
		t.Fatal("expected an error for an absolute-path entry, got nil")
	}
	t.Logf("extractTarGz returned an error (whole extraction refused): %v", err)
	if !strings.Contains(err.Error(), "unsafe name") {
		t.Errorf("error = %v, want it to mention the unsafe name", err)
	}
	if _, statErr := os.Stat("/etc/passwd-clone"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("/etc/passwd-clone exists on the real filesystem — this test environment is contaminated or the check regressed")
	}
	t.Log("confirmed: /etc/passwd-clone does not exist on the real filesystem")
}

// TestExtractTarGz_WindowsDriveAbsoluteRejected: a drive-absolute name
// (`C:/evil.md` or `C:\evil.md`) isn't caught by a plain leading-"/"
// check, since tar names are conventionally "/"-separated. Both the
// colon and the backslash are rejected outright regardless of host OS.
func TestExtractTarGz_WindowsDriveAbsoluteRejected(t *testing.T) {
	for _, name := range []string{`C:/evil.md`, `C:\evil.md`, `wiki\..\..\evil.md`} {
		t.Run(name, func(t *testing.T) {
			body := buildTarGz(t, []tarEntry{regEntry(name, "PWNED")})
			src := writeTarGzFile(t, t.TempDir(), body)
			dest := t.TempDir()
			if err := extractTarGz(src, dest); err == nil {
				t.Fatalf("expected an error for entry name %q, got nil", name)
			}
		})
	}
}

// assertNoSecretBytesAnywhere walks root (recursively) and fails the
// test if any regular file's content contains marker. Used to prove a
// skipped/rejected entry didn't somehow leak external bytes into the
// destination tree by another path.
func assertNoSecretBytesAnywhere(t *testing.T, root, marker string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil //nolint:nilerr // best-effort scan.
		}
		body, rerr := os.ReadFile(p) //nolint:gosec // G304: p comes from WalkDir over a test's own tempdir tree.
		if rerr != nil {
			return nil //nolint:nilerr
		}
		if strings.Contains(string(body), marker) {
			t.Fatalf("found secret marker %q leaked into %s", marker, p)
		}
		return nil
	})
}

// --- hardening: resource caps ---

// TestExtractTarGz_PerFileCapEnforced: a single entry declaring more
// than maxExtractedFileBytes must be rejected rather than written to
// disk unbounded.
func TestExtractTarGz_PerFileCapEnforced(t *testing.T) {
	origCap := maxExtractedFileBytes
	maxExtractedFileBytes = 1024
	t.Cleanup(func() { maxExtractedFileBytes = origCap })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	size := maxExtractedFileBytes + 4096
	if err := tw.WriteHeader(&tar.Header{Name: "huge.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	src := writeTarGzFile(t, t.TempDir(), buf.Bytes())
	dest := t.TempDir()

	err := extractTarGz(src, dest)
	if err == nil {
		t.Fatal("expected an error for an oversize entry")
	}
	if !strings.Contains(err.Error(), "per-file cap") {
		t.Errorf("error = %v, want it to mention the per-file cap", err)
	}
}

// TestExtractTarGz_CumulativeCapEnforced is the decompression-bomb
// regression test: a single decoy entry declaring a size larger than
// the cumulative budget (fed through gzip, which compresses the
// all-zeros payload to almost nothing) must be rejected quickly, not
// after decompressing the full declared size.
func TestExtractTarGz_CumulativeCapEnforced(t *testing.T) {
	origCap := maxExtractedTotalBytes
	maxExtractedTotalBytes = 8192
	t.Cleanup(func() { maxExtractedTotalBytes = origCap })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	size := maxExtractedTotalBytes + 4096
	if err := tw.WriteHeader(&tar.Header{Name: "decoy-not-real-content", Typeflag: tar.TypeReg, Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	src := writeTarGzFile(t, t.TempDir(), buf.Bytes())
	dest := t.TempDir()

	start := time.Now()
	err := extractTarGz(src, dest)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error for a cumulative-oversize archive")
	}
	if !strings.Contains(err.Error(), "cumulative") {
		t.Errorf("error = %v, want it to mention the cumulative cap", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("extractTarGz took %s — the cumulative cap should short-circuit quickly", elapsed)
	}
}

// TestExtractTarGz_EntryCountCapEnforced: many tiny entries, each
// individually far under any byte cap, must still be rejected once
// their count exceeds maxExtractEntries.
func TestExtractTarGz_EntryCountCapEnforced(t *testing.T) {
	origCap := maxExtractEntries
	maxExtractEntries = 10
	t.Cleanup(func() { maxExtractEntries = origCap })

	var entries []tarEntry
	for i := 0; i < maxExtractEntries+5; i++ {
		entries = append(entries, regEntry("f"+string(rune('a'+i))+".md", "x"))
	}
	body := buildTarGz(t, entries)
	src := writeTarGzFile(t, t.TempDir(), body)
	dest := t.TempDir()

	err := extractTarGz(src, dest)
	if err == nil {
		t.Fatal("expected an error once entry count exceeds the cap")
	}
	if !strings.Contains(err.Error(), "entry cap") {
		t.Errorf("error = %v, want it to mention the entry cap", err)
	}
}

// zeroReader is an io.Reader yielding an endless stream of zero bytes,
// used to synthesize a large tar entry without allocating it — mirrors
// internal/update/download_test.go's helper of the same name (a
// different package; small enough that duplicating it here is simpler
// than sharing it).
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// --- direct unit tests of the small building blocks ---

func TestSafeEntryName(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"wiki/index.md", "wiki/index.md", true},
		{"wiki/", "wiki", true},
		{".", ".", true},
		{"", "", false},
		{"/etc/passwd", "", false},
		{"..", "", false},
		{"../escape.md", "", false},
		{"../../escape.md", "", false},
		{"wiki/../../escape.md", "", false},
		{"wiki/../concepts/x.md", "concepts/x.md", true}, // cancels to a legitimate in-tree path
		{"C:/evil.md", "", false},
		{`C:\evil.md`, "", false},
		{`wiki\evil.md`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := safeEntryName(c.name)
			if ok != c.ok {
				t.Fatalf("safeEntryName(%q) ok = %v, want %v", c.name, ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("safeEntryName(%q) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestCumulativeLimitReader_Archive(t *testing.T) {
	data := strings.Repeat("a", 100)
	cr := &cumulativeLimitReader{r: strings.NewReader(data), remaining: 40}

	buf := make([]byte, 4096)
	n, err := cr.Read(buf)
	if err != nil {
		t.Fatalf("first read: unexpected error %v", err)
	}
	if n != 40 {
		t.Fatalf("first read returned %d bytes, want 40", n)
	}
	if cr.exceeded {
		t.Fatal("exceeded should still be false after consuming exactly the budget")
	}

	n, err = cr.Read(buf)
	if n != 0 {
		t.Errorf("read past budget returned %d bytes, want 0", n)
	}
	if !errors.Is(err, errExtractLimitExceeded) {
		t.Fatalf("err = %v, want errExtractLimitExceeded", err)
	}
	if !cr.exceeded {
		t.Fatal("expected exceeded=true once the budget is exhausted")
	}
}

// --- hardening: sparse entries and the on-disk write budget ---

// rawTarHeader returns one 512-byte ustar header block for name with the
// given typeflag and size, checksum included. archive/tar's Writer will
// not emit GNU sparse records, so the sparse tests assemble their
// archives from raw blocks.
func rawTarHeader(t *testing.T, name string, typeflag byte, size int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: size, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	blk := append([]byte(nil), buf.Bytes()[:512]...)
	blk[156] = typeflag
	for i := 148; i < 156; i++ {
		blk[i] = ' '
	}
	var sum int64
	for _, b := range blk {
		sum += int64(b)
	}
	copy(blk[148:], []byte(fmt.Sprintf("%06o\x00 ", sum)))
	return blk
}

// padBlock pads b with zero bytes to a multiple of the 512-byte tar block.
func padBlock(b []byte) []byte {
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}

// paxRecord renders one PAX extended-header record ("<len> key=value\n").
func paxRecord(k, v string) string {
	rec := " " + k + "=" + v + "\n"
	n := len(rec)
	for {
		l := len(strconv.Itoa(n + len(rec)))
		if l+len(rec) == n {
			return strconv.Itoa(n) + rec
		}
		n = l + len(rec)
	}
}

// sparseTarGz builds a gzip'd tar with count GNU PAX 0.1 sparse entries,
// each storing one data byte but declaring logicalSize bytes of content.
func sparseTarGz(t *testing.T, count int, logicalSize int64) []byte {
	t.Helper()
	var raw []byte
	for i := range count {
		name := fmt.Sprintf("sparse-%d.md", i)
		pax := paxRecord("GNU.sparse.major", "0") + paxRecord("GNU.sparse.minor", "1") +
			paxRecord("GNU.sparse.size", strconv.FormatInt(logicalSize, 10)) +
			paxRecord("GNU.sparse.numblocks", "1") + paxRecord("GNU.sparse.map", "0,1")
		raw = append(raw, rawTarHeader(t, "PaxHeaders/"+name, tar.TypeXHeader, int64(len(pax)))...)
		raw = append(raw, padBlock([]byte(pax))...)
		raw = append(raw, rawTarHeader(t, name, tar.TypeReg, 1)...)
		raw = append(raw, padBlock([]byte("x"))...)
	}
	raw = append(raw, make([]byte, 1024)...) // end-of-archive marker
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// dirSize sums the sizes of every regular file under root.
func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// TestExtractTarGz_SparseEntryRefused: a few-hundred-byte archive of
// sparse entries whose logical size is far above the cumulative cap must
// be refused, and must not leave more than the cap on disk.
func TestExtractTarGz_SparseEntryRefused(t *testing.T) {
	origTotal := maxExtractedTotalBytes
	maxExtractedTotalBytes = 1 << 20
	t.Cleanup(func() { maxExtractedTotalBytes = origTotal })

	body := sparseTarGz(t, 4, 4<<20)
	if len(body) > 4096 {
		t.Fatalf("test archive is %d bytes; it should be tiny", len(body))
	}

	// The fixture really is sparse as archive/tar sees it.
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if !isSparseEntry(hdr) || hdr.Size != 4<<20 {
		t.Fatalf("fixture header = %+v, want a sparse entry of %d bytes", hdr, 4<<20)
	}

	dest := t.TempDir()
	err = extractTarGz(writeTarGzFile(t, t.TempDir(), body), dest)
	if err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("extractTarGz err = %v, want a sparse-entry refusal", err)
	}
	if got := dirSize(t, dest); got > maxExtractedTotalBytes {
		t.Errorf("%d bytes on disk after refusal, want <= %d", got, maxExtractedTotalBytes)
	}
}

func TestIsSparseEntry(t *testing.T) {
	cases := []struct {
		name string
		hdr  tar.Header
		want bool
	}{
		{"regular", tar.Header{Typeflag: tar.TypeReg}, false},
		{"other pax", tar.Header{Typeflag: tar.TypeReg, PAXRecords: map[string]string{"path": "a"}}, false},
		{"old gnu", tar.Header{Typeflag: tar.TypeGNUSparse}, true},
		{"pax 1.0", tar.Header{Typeflag: tar.TypeReg, PAXRecords: map[string]string{"GNU.sparse.major": "1"}}, true},
		{"pax 0.0", tar.Header{Typeflag: tar.TypeReg, PAXRecords: map[string]string{"GNU.sparse.offset": "0"}}, true},
	}
	for _, tc := range cases {
		if got := isSparseEntry(&tc.hdr); got != tc.want {
			t.Errorf("%s: isSparseEntry = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestExtractRegularFile_WriteBudget: the on-disk budget is enforced on
// bytes written, independently of the gzip read budget, and bounds what
// lands on disk to the remaining budget plus one detection byte.
func TestExtractRegularFile_WriteBudget(t *testing.T) {
	body := buildTarGz(t, []tarEntry{regEntry("a.md", strings.Repeat("a", 100))})
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	cum := &cumulativeLimitReader{r: gz, remaining: 1 << 20}
	tr := tar.NewReader(cum)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	budget := &extractBudget{remaining: 40}
	err = extractRegularFile(root, "a.md", "a.md", tr, cum, budget)
	if err == nil || !strings.Contains(err.Error(), "cumulative") {
		t.Fatalf("err = %v, want the cumulative cap", err)
	}
	if got := dirSize(t, dest); got > 41 {
		t.Errorf("%d bytes on disk, want <= 41", got)
	}
}
