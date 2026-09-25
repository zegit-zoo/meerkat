package contentsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// objectstore.go is the provider-neutral half of the object-store
// content sources (type: gcs, type: s3). Everything that is about
// serving a bucket — bundle mode versus prefix mode, immutable cache
// keys, conditional reads, size caps, the os.Root-contained tree write,
// the cheap metadata-only probe — lives here once. What differs between
// providers is confined to a storeKind (constants) and an objectStore
// (the client seam), so a new backend is a new adapter, not a new fetch
// path. See docs/design/object-stores.md.

// storedObject is the object metadata the shared paths need. It is a
// small local struct rather than a library type so every adapter can be
// faked in tests without constructing SDK values.
//
// Version is the provider's immutability token for exactly these bytes:
// a GCS generation (decimal), an S3 ETag (opaque). It is compared and
// sent back verbatim, never interpreted.
type storedObject struct {
	Name    string
	Version string
	Size    int64
}

// objectStore is the slice of an object-store client this package uses.
// It exists as an interface for one reason: it is the test seam. Every
// object-store test in this repo runs against a fake implementation —
// there is no test anywhere that needs real credentials, a real bucket,
// or the network. The conformance tests that do talk to a real Garage,
// Versity Gateway or AWS are opt-in through an environment variable and
// skip otherwise.
type objectStore interface {
	// Attrs returns the current metadata for one object.
	Attrs(ctx context.Context, bucket, object string) (storedObject, error)
	// Objects lists every object under prefix, in whatever order the
	// backend yields (the caller sorts).
	Objects(ctx context.Context, bucket, prefix string) ([]storedObject, error)
	// Open reads exactly version of object — never a different one. An
	// adapter must make that a backend-enforced precondition (GCS
	// ifGenerationMatch, S3 If-Match), not a client-side check.
	Open(ctx context.Context, bucket, object, version string) (io.ReadCloser, error)
	io.Closer
}

// maxStoreObjects caps how many objects a prefix mount may list/fetch.
// A prefix is operator-chosen, but a mistyped one ("" instead of
// "kb/live/") can name an entire bucket, and every listed object is
// downloaded before anything is served. Failing loudly at a sane bound
// beats a silent multi-hour startup. Mirrors archive.go's
// maxExtractEntries in spirit; a var so tests can shrink it.
var maxStoreObjects = 20_000

// storeKind is the per-provider vocabulary the shared paths are
// parametrised by. One value per source type, declared next to its
// adapter (gcsKind in gcs.go, s3Kind in s3.go).
type storeKind struct {
	// scheme is the provenance URI scheme ("gcs", "s3") and the cache
	// subdirectory name.
	scheme string
	// span and opKey are the telemetry span name and operation
	// attribute key ("meerkat.gcs" / meerkat.gcs.operation, ...).
	span  string
	opKey attribute.Key
	// objectType and prefixType are the bounded telemetry source types
	// for the two modes.
	objectType, prefixType string
	// newClient constructs the provider client for src. It reads the
	// package-level constructor var at call time so tests can swap it.
	newClient func(ctx context.Context, src Source) (objectStore, error)
	// pinnedVersion returns the version an operator pinned in the
	// config (generation:, etag:), or "" to follow the object.
	pinnedVersion func(src Source) string
	// cacheKey is the material hashed into the cache directory name —
	// everything that distinguishes one store location from another.
	cacheKey func(src Source) string
	// fingerprintSize folds object sizes into the prefix-mode listing
	// fingerprint. GCS generations are unique per write, so (name,
	// generation) is enough; S3 ETags are not guaranteed content-derived
	// under every encryption mode, so (key, etag, size) is used there.
	fingerprintSize bool
	// validate is the shape check for this type (bucket, exactly one of
	// object/prefix, pin sanity), shared by config load and the direct
	// entry points.
	validate func(src Source, p string) error
}

// kindFor returns the storeKind for an object-store source type.
func kindFor(typ string) (storeKind, bool) {
	switch typ {
	case TypeGCS:
		return gcsKind, true
	case TypeS3:
		return s3Kind, true
	}
	return storeKind{}, false
}

// IsObjectStore reports whether s is served from an object store
// (type: gcs or type: s3) — the sources that carry a version token,
// can be probed cheaply, and may take a refresh block.
func (s Source) IsObjectStore() bool {
	_, ok := kindFor(s.Type)
	return ok
}

// FetchObject resolves any object-store source (type: gcs, type: s3) to
// a local content-repo-layout directory and returns the version token
// identifying exactly what was served. It is the single entry point
// runtime resolution and hot reload use; FetchGCS / FetchS3 are the
// per-type names.
func FetchObject(ctx context.Context, src Source) (dir, version string, err error) {
	kind, ok := kindFor(src.Type)
	if !ok {
		return "", "", fmt.Errorf("FetchObject: type %q is not an object-store source", src.Type)
	}
	return fetchStore(ctx, src, kind)
}

// ObjectVersion returns the version token an object-store source
// resolves to right now, using metadata calls only. It is the cheap
// half of hot reload; see probe.go for the contract.
func ObjectVersion(ctx context.Context, src Source) (string, error) {
	kind, ok := kindFor(src.Type)
	if !ok {
		return "", fmt.Errorf("ObjectVersion: type %q is not an object-store source", src.Type)
	}
	return storeVersion(ctx, src, kind)
}

// ObjectProvenance formats the kb_source string for an object-store
// source at version: "<scheme>://<bucket>/<object>@<version>" for a
// bundle, "<scheme>://<bucket>/<prefix>*@<version>" for a prefix tree.
//
// Like URLProvenance (and unlike "disk:<path>"), the token after @ is a
// checked property of the content actually served, not a label: every
// fetch is a conditional read that cannot return a different version.
func ObjectProvenance(src Source, version string) string {
	kind, ok := kindFor(src.Type)
	if !ok {
		return src.Type + ":" + src.Bucket
	}
	return kind.provenance(src, version)
}

// provenance formats the kb_source string for src at version under
// this kind's scheme, whatever src.Type says — the per-type helpers
// (GCSProvenance, S3Provenance) are the type.
func (k storeKind) provenance(src Source, version string) string {
	if src.Object != "" {
		return fmt.Sprintf("%s://%s/%s@%s", k.scheme, src.Bucket, src.Object, version)
	}
	return fmt.Sprintf("%s://%s/%s*@%s", k.scheme, src.Bucket, src.Prefix, version)
}

// storeCacheDir returns the cache directory for a resolved object-store
// source. version is the token that makes the entry immutable.
//
// The location is hashed rather than used as a path: object names are
// near-arbitrary UTF-8 (slashes, dots, leading "..", ":" on Windows),
// so pasting one into a filesystem path is exactly the class of bug
// archive.go's safeEntryName exists to prevent. The hash also keeps the
// path length bounded. version is used as-is when it is a plain token
// and hashed otherwise, for the same reason.
func storeCacheDir(src Source, kind storeKind, version string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	sum := sha256.Sum256([]byte(kind.cacheKey(src)))
	return filepath.Join(base, "meerkat", "content", kind.scheme, hex.EncodeToString(sum[:])[:32], safeVersionSegment(version)), nil
}

// safeVersionSegment returns version unchanged when it is a plain
// [A-Za-z0-9._-] token (a generation, a hex fingerprint, an ETag), and
// a hash of it otherwise.
func safeVersionSegment(version string) string {
	plain := version != "" && version != "." && version != ".."
	for _, c := range version {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			plain = false
		}
	}
	if plain {
		return version
	}
	sum := sha256.Sum256([]byte(version))
	return hex.EncodeToString(sum[:])[:32]
}

// fetchStore is the shared body of FetchGCS / FetchS3.
//
// Skip-if-cached comes first in both modes, and a cache hit does no
// network I/O beyond the one metadata call needed to learn the current
// version — or none at all when the version is pinned in the config.
func fetchStore(ctx context.Context, src Source, kind storeKind) (dir, version string, err error) {
	// Re-check the shape here as well as in Validate: the fetch paths
	// below must not be reachable with a half-specified source if this
	// is ever called directly.
	if err := kind.validate(src, "content"); err != nil {
		return "", "", err
	}
	client, err := kind.newClient(ctx, src)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = client.Close() }()

	if src.Object != "" {
		return fetchBundle(ctx, client, src, kind)
	}
	return fetchPrefixTree(ctx, client, src, kind)
}

// fetchBundle implements object mode: one .tar.gz bundle, cached by the
// object's version token.
func fetchBundle(ctx context.Context, client objectStore, src Source, kind storeKind) (dir, version string, err error) {
	version = kind.pinnedVersion(src)
	if version == "" {
		attrs, aerr := storeOp(ctx, kind, "attrs", func() (storedObject, error) {
			return client.Attrs(ctx, src.Bucket, src.Object)
		})
		if aerr != nil {
			return "", "", fmt.Errorf("%s://%s/%s: %w", kind.scheme, src.Bucket, src.Object, aerr)
		}
		version = attrs.Version
	}
	if version == "" {
		return "", "", fmt.Errorf("%s://%s/%s: backend returned no version token for the object", kind.scheme, src.Bucket, src.Object)
	}

	cacheDir, err := storeCacheDir(src, kind, version)
	if err != nil {
		return "", "", err
	}
	if isCacheComplete(cacheDir) {
		// Immutable by version: a restart on the same version is free,
		// exactly as a type: url restart on the same digest is.
		telemetry.Record(ctx).CacheLookup(kind.objectType, telemetry.CacheHit)
		return cacheDir, version, nil
	}
	telemetry.Record(ctx).CacheLookup(kind.objectType, telemetry.CacheMiss)

	tmpFile, digest, err := downloadStoreToTemp(ctx, client, kind, src.Bucket, src.Object, version)
	if err != nil {
		return "", "", fmt.Errorf("%s://%s/%s@%s: %w", kind.scheme, src.Bucket, src.Object, version, err)
	}
	defer func() { _ = os.Remove(tmpFile) }()
	if info, serr := os.Stat(tmpFile); serr == nil {
		telemetry.Record(ctx).Downloaded(kind.objectType, info.Size())
	}

	// sha256 is optional for an object store (the version already pins
	// the bytes), but when an operator does pin one, it is verified
	// BEFORE anything is extracted or cached — same rule as type: url.
	if src.SHA256 != "" && digest != strings.ToLower(src.SHA256) {
		return "", "", fmt.Errorf("sha256 mismatch for %s://%s/%s@%s: got %s, want %s — refusing to extract or cache",
			kind.scheme, src.Bucket, src.Object, version, digest, strings.ToLower(src.SHA256))
	}

	err = populateCacheDir(cacheDir, kind.provenance(src, version), func(tmpDir string) error {
		return extractTarGz(tmpFile, tmpDir)
	})
	if err != nil {
		return "", "", err
	}
	return cacheDir, version, nil
}

// fetchPrefixTree implements prefix mode: every object under a prefix,
// laid out as a directory tree, cached by a fingerprint over the whole
// listing.
func fetchPrefixTree(ctx context.Context, client objectStore, src Source, kind storeKind) (dir, version string, err error) {
	objs, err := storeOp(ctx, kind, "list", func() ([]storedObject, error) {
		return client.Objects(ctx, src.Bucket, src.Prefix)
	})
	if err != nil {
		return "", "", fmt.Errorf("%s://%s/%s*: %w", kind.scheme, src.Bucket, src.Prefix, err)
	}
	objs, err = fetchableListing(objs, src, kind, true)
	if err != nil {
		return "", "", err
	}
	version = listingFingerprint(objs, kind.fingerprintSize)

	cacheDir, err := storeCacheDir(src, kind, version)
	if err != nil {
		return "", "", err
	}
	if isCacheComplete(cacheDir) {
		telemetry.Record(ctx).CacheLookup(kind.prefixType, telemetry.CacheHit)
		return cacheDir, version, nil
	}
	telemetry.Record(ctx).CacheLookup(kind.prefixType, telemetry.CacheMiss)

	ctx, span := telemetry.Span(ctx, kind.span,
		kind.opKey.String("read"),
		telemetry.KeySourceObjects.Int(len(objs)))
	err = populateCacheDir(cacheDir, kind.provenance(src, version), func(tmpDir string) error {
		return downloadStoreTree(ctx, client, kind, src, objs, tmpDir)
	})
	if err != nil {
		telemetry.Fail(span, telemetry.OutcomeError)
		return "", "", err
	}
	span.SetAttributes(telemetry.Outcome(telemetry.OutcomeOK))
	span.End()
	return cacheDir, version, nil
}

// storeVersion is the shared body of GCSVersion / S3Version: the
// version token the source resolves to right now, from metadata alone.
// A pinned version short-circuits with no call at all: the answer
// cannot change, so asking would be a metadata request whose result is
// already known.
func storeVersion(ctx context.Context, src Source, kind storeKind) (string, error) {
	if err := kind.validate(src, "content"); err != nil {
		return "", err
	}
	if src.Object != "" {
		if pinned := kind.pinnedVersion(src); pinned != "" {
			return pinned, nil
		}
	}

	client, err := kind.newClient(ctx, src)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()

	if src.Object != "" {
		attrs, aerr := client.Attrs(ctx, src.Bucket, src.Object)
		if aerr != nil {
			return "", fmt.Errorf("%s://%s/%s: %w", kind.scheme, src.Bucket, src.Object, aerr)
		}
		if attrs.Version == "" {
			return "", fmt.Errorf("%s://%s/%s: backend returned no version token for the object", kind.scheme, src.Bucket, src.Object)
		}
		return attrs.Version, nil
	}

	objs, lerr := client.Objects(ctx, src.Bucket, src.Prefix)
	if lerr != nil {
		return "", fmt.Errorf("%s://%s/%s*: %w", kind.scheme, src.Bucket, src.Prefix, lerr)
	}
	// quiet: the same skip decisions the fetch makes, but without the
	// per-object stderr warning. A probe runs every interval forever, and
	// one permanently oddly-named object in a shared bucket must not
	// produce a warning line per minute for the life of the process. The
	// fetch that follows an actual change still warns, once.
	objs, err = fetchableListing(objs, src, kind, false)
	if err != nil {
		return "", err
	}
	return listingFingerprint(objs, kind.fingerprintSize), nil
}

// fetchableListing filters a prefix listing down to the objects the
// tree will hold, enforces the object cap, and sorts by name so the
// fingerprint is order-independent.
func fetchableListing(objs []storedObject, src Source, kind storeKind, warn bool) ([]storedObject, error) {
	objs = keepFetchableObjects(objs, src.Prefix, kind.scheme, warn)
	if len(objs) > maxStoreObjects {
		return nil, fmt.Errorf("%s://%s/%s*: prefix matches %d objects, over the %d-object cap; refusing (narrow the prefix)",
			kind.scheme, src.Bucket, src.Prefix, len(objs), maxStoreObjects)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
	return objs, nil
}

// storeOp wraps one object-store call in a span.
//
// The span names the OPERATION — attrs, list, read — and nothing else.
// No bucket, no object name, no prefix, no version, no endpoint: a
// bucket name is a deployment's storage layout, an object name is
// frequently the document's own title, and both would be exported
// off-box. What is left is what an operator needs from a trace anyway:
// which kind of call was slow, and how many objects it touched.
func storeOp[T any](ctx context.Context, kind storeKind, operation string, fn func() (T, error)) (T, error) {
	_, span := telemetry.Span(ctx, kind.span, kind.opKey.String(operation))
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
// path passes true; the reconciliation probe passes false, because it
// re-lists on every interval and would otherwise emit the same line
// about the same permanently-misnamed object forever.
func keepFetchableObjects(objs []storedObject, prefix, scheme string, warn bool) []storedObject {
	out := objs[:0:0]
	for _, o := range objs {
		if strings.HasSuffix(o.Name, "/") {
			continue
		}
		if _, ok := objectRelPath(o.Name, prefix); !ok {
			if warn {
				fmt.Fprintf(os.Stderr, "meerkat: skipping %s object with unsafe name %q\n", scheme, o.Name)
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

// listingFingerprint hashes the sorted (name, version[, size]) tuples of
// a listing into the cache-key token for prefix mode. Any object added,
// removed, or overwritten changes it — which is what makes a prefix
// mount's cache invalidate on content change without ever having to
// trust a mutable path.
//
// Without withSize the bytes hashed are exactly what the GCS-only
// implementation hashed, so an upgrade does not invalidate a GCS
// prefix cache or report a spurious change to hot reload.
func listingFingerprint(objs []storedObject, withSize bool) string {
	h := sha256.New()
	for _, o := range objs {
		if withSize {
			fmt.Fprintf(h, "%s\x00%s\x00%d\n", o.Name, o.Version, o.Size)
		} else {
			fmt.Fprintf(h, "%s\x00%s\n", o.Name, o.Version)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// downloadStoreTree writes every object into destDir at its path
// relative to the prefix, enforcing the same per-file and cumulative
// size caps archive.go enforces on an extracted tarball.
func downloadStoreTree(ctx context.Context, client objectStore, kind storeKind, src Source, objs []storedObject, destDir string) error {
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
		n, err := downloadStoreObjectInto(ctx, client, kind, root, src.Bucket, o, rel, remaining)
		if err != nil {
			return err
		}
		remaining -= n
		if remaining <= 0 {
			return fmt.Errorf("%s://%s/%s*: exceeds the %d-byte cumulative size cap; refusing", kind.scheme, src.Bucket, src.Prefix, maxExtractedTotalBytes)
		}
	}
	return nil
}

// downloadStoreObjectInto writes one object to rel within root and
// returns how many bytes it wrote.
func downloadStoreObjectInto(ctx context.Context, client objectStore, kind storeKind, root *os.Root, bucket string, o storedObject, rel string, budget int64) (int64, error) {
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return 0, fmt.Errorf("mkdir %q: %w", dir, err)
		}
	}
	rc, err := client.Open(ctx, bucket, o.Name, o.Version)
	if err != nil {
		return 0, fmt.Errorf("%s://%s/%s@%s: %w", kind.scheme, bucket, o.Name, o.Version, err)
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
		return 0, fmt.Errorf("download %s://%s/%s: %w", kind.scheme, bucket, o.Name, err)
	}
	if n > limit {
		return 0, fmt.Errorf("%s://%s/%s exceeds the %d-byte per-file cap; refusing", kind.scheme, bucket, o.Name, maxExtractedFileBytes)
	}
	return n, out.Close()
}

// downloadStoreToTemp streams one object version to a temp file,
// enforcing maxDownloadBytes, and returns the path plus the hex sha256
// of exactly the bytes written. The caller removes the file.
func downloadStoreToTemp(ctx context.Context, client objectStore, kind storeKind, bucket, object, version string) (tmpPath, sha256hex string, err error) {
	rc, err := client.Open(ctx, bucket, object, version)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rc.Close() }()

	tmp, err := os.CreateTemp("", "meerkat-"+kind.scheme+"-*.tar.gz")
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
