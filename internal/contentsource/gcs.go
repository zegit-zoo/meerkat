package contentsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// gcs.go implements type: gcs — a Google Cloud Storage bucket as a
// runtime content source, in two modes:
//
//   - object: a single .tar.gz bundle, the GCS analogue of type: url.
//     Where type: url is keyed (and verified) by a mandatory sha256,
//     this is keyed by the object's GENERATION: GCS assigns a new
//     generation to every write, so a (bucket, object, generation)
//     triple names immutable bytes just as a digest does. The fetch is
//     a conditional read (ifGenerationMatch + an explicit generation)
//     so the bytes retrieved can only ever be the generation the cache
//     entry is named after — never a newer object that replaced it
//     mid-fetch.
//   - prefix: an object prefix served as a directory tree. There is no
//     single generation to key on, so the cache is keyed by a
//     fingerprint over every listed object's (name, generation) pair:
//     any add, delete, or overwrite under the prefix changes the
//     fingerprint and therefore the cache entry. Each object is then
//     fetched with the same conditional read.
//
// Authentication is Application Default Credentials (and therefore
// Workload Identity Federation, GKE/Cloud Run metadata, an impersonated
// principal, or a developer's `gcloud auth application-default login`),
// resolved by the Google client library from the ambient environment.
// meerkat never reads, stores, or accepts a static service-account key:
// the schema has no field to name one (see Source.Bucket's comment).

// maxGCSObjects caps how many objects a prefix mount may list/fetch.
// A prefix is operator-chosen, but a mistyped one ("" instead of
// "kb/live/") can name an entire bucket, and every listed object is
// downloaded before anything is served. Failing loudly at a sane bound
// beats a silent multi-hour startup. Mirrors archive.go's
// maxExtractEntries in spirit; a var so tests can shrink it.
var maxGCSObjects = 20_000

// gcsObject is the object metadata FetchGCS needs. It is deliberately
// a small local struct rather than *storage.ObjectAttrs so the seam
// below (gcsAPI) can be faked in tests without constructing library
// types.
type gcsObject struct {
	Name       string
	Generation int64
	Size       int64
}

// gcsAPI is the slice of cloud.google.com/go/storage this package uses.
// It exists as an interface for one reason: it is the test seam. Every
// GCS test in this repo runs against a fake implementation — there is
// no test anywhere that needs real credentials, a real bucket, or the
// network.
type gcsAPI interface {
	// Attrs returns the current metadata for one object.
	Attrs(ctx context.Context, bucket, object string) (gcsObject, error)
	// Objects lists every object under prefix, in whatever order the
	// backend yields (FetchGCS sorts).
	Objects(ctx context.Context, bucket, prefix string) ([]gcsObject, error)
	// Open reads exactly generation of object — never a different one.
	Open(ctx context.Context, bucket, object string, generation int64) (io.ReadCloser, error)
	io.Closer
}

// newGCSClient constructs the client FetchGCS uses. A package var so
// tests can substitute a fake without a build tag or an exported
// test-only knob; production always gets the real ADC-backed client.
var newGCSClient = func(ctx context.Context) (gcsAPI, error) { return newStorageAPI(ctx) }

// storageAPI is the real gcsAPI, over cloud.google.com/go/storage.
type storageAPI struct{ c *storage.Client }

func newStorageAPI(ctx context.Context) (gcsAPI, error) {
	// storage.NewClient with no option.ClientOption resolves credentials
	// via Application Default Credentials: GOOGLE_APPLICATION_CREDENTIALS
	// (including a Workload Identity Federation external_account config),
	// gcloud's application-default credentials, or the GCE/GKE/Cloud Run
	// metadata server. Passing no credential options is what keeps every
	// one of those paths available — and keeps a static key from being
	// something meerkat could be asked to load.
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("google cloud storage: %w — credentials resolve via Application Default Credentials; "+
			"run `gcloud auth application-default login`, or attach a workload identity / service account to the workload", err)
	}
	return storageAPI{c: c}, nil
}

func (s storageAPI) Attrs(ctx context.Context, bucket, object string) (gcsObject, error) {
	a, err := s.c.Bucket(bucket).Object(object).Attrs(ctx)
	if err != nil {
		return gcsObject{}, err
	}
	return gcsObject{Name: a.Name, Generation: a.Generation, Size: a.Size}, nil
}

func (s storageAPI) Objects(ctx context.Context, bucket, prefix string) ([]gcsObject, error) {
	it := s.c.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var out []gcsObject
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(out) >= maxGCSObjects {
			return nil, fmt.Errorf("prefix %q matches more than %d objects; refusing (narrow the prefix)", prefix, maxGCSObjects)
		}
		out = append(out, gcsObject{Name: a.Name, Generation: a.Generation, Size: a.Size})
	}
}

func (s storageAPI) Open(ctx context.Context, bucket, object string, generation int64) (io.ReadCloser, error) {
	// SECURITY / correctness: both the explicit generation and the
	// ifGenerationMatch precondition are set. The generation selector
	// asks for exactly those bytes; the precondition makes the request
	// fail (rather than silently serve something else) if the backend
	// could not honour that. Together they give immutable retrieval: the
	// content written into a cache entry named <generation> cannot be
	// the content of any other generation.
	obj := s.c.Bucket(bucket).Object(object).
		Generation(generation).
		If(storage.Conditions{GenerationMatch: generation})
	return obj.NewReader(ctx)
}

func (s storageAPI) Close() error { return s.c.Close() }

// gcsCacheDir returns the cache directory for a resolved gcs source.
// version is the generation (object mode) or the listing fingerprint
// (prefix mode) — the token that makes the entry immutable.
//
// The bucket/object pair is hashed rather than used as a path: object
// names are near-arbitrary UTF-8 (slashes, dots, leading "..", ":" on
// Windows), so pasting one into a filesystem path is exactly the class
// of bug archive.go's safeEntryName exists to prevent. The hash also
// keeps the path length bounded.
func gcsCacheDir(src Source, version string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	mode, target := "object", src.Object
	if src.Object == "" {
		mode, target = "prefix", src.Prefix
	}
	sum := sha256.Sum256([]byte(src.Bucket + "\x00" + mode + "\x00" + target))
	return filepath.Join(base, "meerkat", "content", "gcs", hex.EncodeToString(sum[:])[:32], version), nil
}

// GCSProvenance formats the kb_source string reported for a type: gcs
// source: "gcs://<bucket>/<object>@<generation>" for a bundle, or
// "gcs://<bucket>/<prefix>*@<fingerprint>" for a prefix tree.
//
// Like URLProvenance (and unlike "disk:<path>"), the token after @ is a
// checked property of the content actually served, not a label: the
// fetch below is a conditional read that cannot return a different
// generation.
func GCSProvenance(src Source, version string) string {
	if src.Object != "" {
		return fmt.Sprintf("gcs://%s/%s@%s", src.Bucket, src.Object, version)
	}
	return fmt.Sprintf("gcs://%s/%s*@%s", src.Bucket, src.Prefix, version)
}

// FetchGCS resolves a type: gcs source to a local content-repo-layout
// directory ready for kbdir.ConfigureLayout/FSLayout, and returns the
// version token identifying exactly what was served (see GCSProvenance).
//
// Skip-if-cached comes first in both modes, and a cache hit does no
// network I/O beyond the one metadata call needed to learn the current
// generation — or none at all when generation: is pinned in the config.
func FetchGCS(ctx context.Context, src Source) (dir, version string, err error) {
	if src.Type != TypeGCS {
		return "", "", fmt.Errorf("FetchGCS: type %q is not %q", src.Type, TypeGCS)
	}
	// Re-check the shape here as well as in Validate: FetchGCS must not
	// be a way to reach the fetch paths below with a half-specified
	// source if it is ever called directly.
	if err := src.validateGCS("content"); err != nil {
		return "", "", err
	}

	client, err := newGCSClient(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = client.Close() }()

	if src.Object != "" {
		return fetchGCSObject(ctx, client, src)
	}
	return fetchGCSPrefix(ctx, client, src)
}

// fetchGCSObject implements object mode: one .tar.gz bundle, cached by
// object generation.
func fetchGCSObject(ctx context.Context, client gcsAPI, src Source) (dir, version string, err error) {
	generation := src.Generation
	if generation == 0 {
		attrs, aerr := gcsOp(ctx, "attrs", func() (gcsObject, error) {
			return client.Attrs(ctx, src.Bucket, src.Object)
		})
		if aerr != nil {
			return "", "", fmt.Errorf("gcs://%s/%s: %w", src.Bucket, src.Object, aerr)
		}
		generation = attrs.Generation
	}
	version = strconv.FormatInt(generation, 10)

	cacheDir, err := gcsCacheDir(src, version)
	if err != nil {
		return "", "", err
	}
	if isCacheComplete(cacheDir) {
		// Immutable by generation: a restart on the same generation is
		// free, exactly as a type: url restart on the same digest is.
		telemetry.Record(ctx).CacheLookup(telemetry.SourceGCSObject, telemetry.CacheHit)
		return cacheDir, version, nil
	}
	telemetry.Record(ctx).CacheLookup(telemetry.SourceGCSObject, telemetry.CacheMiss)

	tmpFile, digest, err := downloadGCSToTemp(ctx, client, src.Bucket, src.Object, generation)
	if err != nil {
		return "", "", fmt.Errorf("gcs://%s/%s@%d: %w", src.Bucket, src.Object, generation, err)
	}
	defer func() { _ = os.Remove(tmpFile) }()
	if info, serr := os.Stat(tmpFile); serr == nil {
		telemetry.Record(ctx).Downloaded(telemetry.SourceGCSObject, info.Size())
	}

	// sha256 is optional for gcs (the generation already pins the bytes),
	// but when an operator does pin one, it is verified BEFORE anything
	// is extracted or cached — same rule as type: url.
	if src.SHA256 != "" && digest != strings.ToLower(src.SHA256) {
		return "", "", fmt.Errorf("sha256 mismatch for gcs://%s/%s@%d: got %s, want %s — refusing to extract or cache",
			src.Bucket, src.Object, generation, digest, strings.ToLower(src.SHA256))
	}

	err = populateCacheDir(cacheDir, GCSProvenance(src, version), func(tmpDir string) error {
		return extractTarGz(tmpFile, tmpDir)
	})
	if err != nil {
		return "", "", err
	}
	return cacheDir, version, nil
}

// fetchGCSPrefix implements prefix mode: every object under a prefix,
// laid out as a directory tree, cached by a fingerprint over the whole
// listing's (name, generation) pairs.
func fetchGCSPrefix(ctx context.Context, client gcsAPI, src Source) (dir, version string, err error) {
	objs, err := gcsOp(ctx, "list", func() ([]gcsObject, error) {
		return client.Objects(ctx, src.Bucket, src.Prefix)
	})
	if err != nil {
		return "", "", fmt.Errorf("gcs://%s/%s*: %w", src.Bucket, src.Prefix, err)
	}
	objs = keepFetchableObjects(objs, src.Prefix, true)
	if len(objs) > maxGCSObjects {
		return "", "", fmt.Errorf("gcs://%s/%s*: prefix matches %d objects, over the %d-object cap; refusing (narrow the prefix)",
			src.Bucket, src.Prefix, len(objs), maxGCSObjects)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
	version = listingFingerprint(objs)

	cacheDir, err := gcsCacheDir(src, version)
	if err != nil {
		return "", "", err
	}
	if isCacheComplete(cacheDir) {
		telemetry.Record(ctx).CacheLookup(telemetry.SourceGCSPrefix, telemetry.CacheHit)
		return cacheDir, version, nil
	}
	telemetry.Record(ctx).CacheLookup(telemetry.SourceGCSPrefix, telemetry.CacheMiss)

	ctx, span := telemetry.Span(ctx, telemetry.SpanGCS,
		telemetry.KeyGCSOperation.String("read"),
		telemetry.KeySourceObjects.Int(len(objs)))
	err = populateCacheDir(cacheDir, GCSProvenance(src, version), func(tmpDir string) error {
		return downloadGCSTree(ctx, client, src, objs, tmpDir)
	})
	if err != nil {
		telemetry.Fail(span, telemetry.OutcomeError)
		return "", "", err
	}
	span.SetAttributes(telemetry.Outcome(telemetry.OutcomeOK))
	span.End()
	return cacheDir, version, nil
}

// gcsOp wraps one Google Cloud Storage call in a span.
//
// The span names the OPERATION — attrs, list, read — and nothing else.
// No bucket, no object name, no prefix, no generation: a bucket name is
// a deployment's storage layout, an object name is frequently the
// document's own title, and both would be exported off-box. What is left
// is what an operator needs from a trace anyway: which kind of call was
// slow, and how many objects it touched.
func gcsOp[T any](ctx context.Context, operation string, fn func() (T, error)) (T, error) {
	_, span := telemetry.Span(ctx, telemetry.SpanGCS,
		telemetry.KeyGCSOperation.String(operation))
	v, err := fn()
	if err != nil {
		telemetry.Fail(span, telemetry.OutcomeError)
		return v, err
	}
	span.SetAttributes(telemetry.Outcome(telemetry.OutcomeOK))
	span.End()
	return v, nil
}

// keepFetchableObjects drops entries that aren't files to serve: the
// "directory placeholder" objects (a trailing slash, zero bytes) some
// tools create, and anything whose name relative to the prefix isn't a
// safe relative path. An unsafe name is skipped rather than fatal — a
// single oddly-named object in a shared bucket must not make the whole
// collection unserveable — and os.Root backs the write side regardless.
//
// warn selects whether a skipped name is reported on stderr. The fetch
// path passes true; the reconciliation probe (see probe.go) passes
// false, because it re-lists on every interval and would otherwise emit
// the same line about the same permanently-misnamed object forever.
func keepFetchableObjects(objs []gcsObject, prefix string, warn bool) []gcsObject {
	out := objs[:0:0]
	for _, o := range objs {
		if strings.HasSuffix(o.Name, "/") {
			continue
		}
		if _, ok := objectRelPath(o.Name, prefix); !ok {
			if warn {
				fmt.Fprintf(os.Stderr, "meerkat: skipping gcs object with unsafe name %q\n", o.Name)
			}
			continue
		}
		out = append(out, o)
	}
	return out
}

// objectRelPath maps a full object name to its path within the mounted
// tree — the name with prefix stripped — validated by the same rules
// archive.go applies to tar entry names.
func objectRelPath(name, prefix string) (string, bool) {
	rel := strings.TrimPrefix(name, prefix)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return "", false
	}
	clean, ok := safeEntryName(rel)
	if !ok || clean == "." {
		return "", false
	}
	return clean, true
}

// listingFingerprint hashes the sorted (name, generation) pairs of a
// listing into the cache-key token for prefix mode. Any object added,
// removed, or overwritten changes it — which is what makes a prefix
// mount's cache invalidate on content change without ever having to
// trust a mutable path.
func listingFingerprint(objs []gcsObject) string {
	h := sha256.New()
	for _, o := range objs {
		fmt.Fprintf(h, "%s\x00%d\n", o.Name, o.Generation)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// downloadGCSTree writes every object into destDir at its path relative
// to the prefix, enforcing the same per-file and cumulative size caps
// archive.go enforces on an extracted tarball.
func downloadGCSTree(ctx context.Context, client gcsAPI, src Source, objs []gcsObject, destDir string) error {
	// SECURITY: every write goes through an os.Root rooted at destDir,
	// for the same reason extractTarGz does — an object name is remote,
	// operator-influenced input, and a string-joined path is not a
	// containment boundary.
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("open destination: %w", err)
	}
	defer func() { _ = root.Close() }()

	remaining := maxExtractedTotalBytes
	for _, o := range objs {
		rel, ok := objectRelPath(o.Name, src.Prefix)
		if !ok {
			continue // already filtered; belt and braces.
		}
		n, err := downloadGCSObjectInto(ctx, client, root, src.Bucket, o, rel, remaining)
		if err != nil {
			return err
		}
		remaining -= n
		if remaining <= 0 {
			return fmt.Errorf("gcs://%s/%s*: exceeds the %d-byte cumulative size cap; refusing", src.Bucket, src.Prefix, maxExtractedTotalBytes)
		}
	}
	return nil
}

// downloadGCSObjectInto writes one object to rel within root and
// returns how many bytes it wrote.
func downloadGCSObjectInto(ctx context.Context, client gcsAPI, root *os.Root, bucket string, o gcsObject, rel string, budget int64) (int64, error) {
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return 0, fmt.Errorf("mkdir %q: %w", dir, err)
		}
	}
	rc, err := client.Open(ctx, bucket, o.Name, o.Generation)
	if err != nil {
		return 0, fmt.Errorf("gcs://%s/%s@%d: %w", bucket, o.Name, o.Generation, err)
	}
	defer func() { _ = rc.Close() }()

	out, err := root.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create %q: %w", rel, err)
	}
	defer func() { _ = out.Close() }()

	limit := min(maxExtractedFileBytes, budget)
	n, err := io.Copy(out, io.LimitReader(rc, limit+1))
	if err != nil {
		return 0, fmt.Errorf("download gcs://%s/%s: %w", bucket, o.Name, err)
	}
	if n > limit {
		return 0, fmt.Errorf("gcs://%s/%s exceeds the %d-byte per-file cap; refusing", bucket, o.Name, maxExtractedFileBytes)
	}
	return n, out.Close()
}

// downloadGCSToTemp streams one object generation to a temp file,
// enforcing maxDownloadBytes, and returns the path plus the hex sha256
// of exactly the bytes written. The caller removes the file.
func downloadGCSToTemp(ctx context.Context, client gcsAPI, bucket, object string, generation int64) (tmpPath, sha256hex string, err error) {
	rc, err := client.Open(ctx, bucket, object, generation)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rc.Close() }()

	tmp, err := os.CreateTemp("", "meerkat-gcs-*.tar.gz")
	if err != nil {
		return "", "", fmt.Errorf("create tempfile: %w", err)
	}
	tmpPath = tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()
	defer func() { _ = tmp.Close() }()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(rc, maxDownloadBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("download: %w", err)
	}
	if n > maxDownloadBytes {
		return "", "", fmt.Errorf("exceeds the %d-byte download cap; refusing", maxDownloadBytes)
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	success = true
	return tmpPath, hex.EncodeToString(h.Sum(nil)), nil
}

// populateCacheDir runs fill against a fresh sibling temp directory and,
// only if that succeeds, marks it complete and atomically renames it
// into place at cacheDir. It is FetchURL's cache-finalisation dance
// factored out and shared: a half-populated directory must never be
// visible at the cache path, and never be treated as a cache hit if it
// somehow is (see completionMarker / isCacheComplete).
//
// marker is written into the completion marker file as a human-readable
// record of what the entry holds.
func populateCacheDir(cacheDir, marker string, fill func(tmpDir string) error) error {
	cacheParent := filepath.Dir(cacheDir)
	if err := os.MkdirAll(cacheParent, 0o750); err != nil {
		return fmt.Errorf("create cache parent dir: %w", err)
	}
	// A sibling of cacheDir, not os.TempDir(): the final step is an
	// os.Rename, which is only atomic within one filesystem.
	tmpDir, err := os.MkdirTemp(cacheParent, ".fetch-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := fill(tmpDir); err != nil {
		return err
	}
	// Written last, inside the not-yet-visible tmpDir, so the rename
	// below moves a tree that already contains it.
	if err := os.WriteFile(filepath.Join(tmpDir, completionMarker), []byte(marker+"\n"), 0o600); err != nil {
		return fmt.Errorf("write completion marker: %w", err)
	}
	// Best-effort cleanup of any earlier incomplete attempt at this exact
	// path (never a cache hit — see isCacheComplete) so Rename can't fail
	// on an existing non-empty directory.
	_ = os.RemoveAll(cacheDir)
	if err := os.Rename(tmpDir, cacheDir); err != nil {
		return fmt.Errorf("finalize cache dir: %w", err)
	}
	succeeded = true
	return nil
}
