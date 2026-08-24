package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// GCSStore is a memory store backed by a Google Cloud Storage prefix.
//
// # Optimistic locking, for real
//
// Unlike the local backend, whose compare-and-swap is a mutex inside
// one process, this one pushes the precondition down to the backend:
//
//	create  ->  ifGenerationMatch: 0   (succeeds only if nothing is there)
//	update  ->  ifGenerationMatch: N   (succeeds only against revision N)
//
// GCS evaluates both server-side and fails the write with 412 otherwise,
// so several meerkat replicas — or several regions — can share one
// memory store and still never lose a write. That is why this backend
// exists at all, and why the design doc points a multi-writer
// deployment at it rather than at a shared NFS directory.
//
// Authentication is Application Default Credentials, exactly as
// internal/contentsource's read-side GCS support: there is no field
// anywhere in the schema for a static service-account key.
type GCSStore struct {
	api    gcsMemoryAPI
	bucket string
	// prefix always ends in "/" (or is empty), so key joining is a
	// concatenation with no separator arithmetic at the call sites.
	prefix string
	// owned marks an api this store constructed and must close.
	owned bool
}

// gcsObject is the object metadata this package needs. A small local
// struct, not *storage.ObjectAttrs, so the seam below can be faked
// without constructing library types — the same shape (and the same
// reason) as internal/contentsource's gcsObject.
type gcsObject struct {
	Name       string
	Generation int64
}

// gcsMemoryAPI is the slice of cloud.google.com/go/storage this package
// uses. It exists as an interface for one reason: it is the test seam.
// Every GCS test in this repo runs against a fake — there is no test
// anywhere that needs credentials, a bucket, or the network.
type gcsMemoryAPI interface {
	// List returns every object under prefix.
	List(ctx context.Context, bucket, prefix string) ([]gcsObject, error)
	// Read returns one object's bytes and the generation they were read
	// at. generation of 0 means "whatever is current".
	Read(ctx context.Context, bucket, object string, generation int64) ([]byte, int64, error)
	// Write stores body at object under a generation precondition, and
	// returns the generation created. ifGeneration of 0 means "the object
	// must not exist"; a positive value means "the object must be exactly
	// this generation". A precondition failure must be reported as
	// errGCSPrecondition.
	Write(ctx context.Context, bucket, object string, body []byte, ifGeneration int64) (int64, error)
	// WriteUnconditional stores body at object with no precondition. Used
	// only for staging artifacts, which supersede rather than collide.
	WriteUnconditional(ctx context.Context, bucket, object string, body []byte) error
	io.Closer
}

// errGCSPrecondition is what an implementation returns when the backend
// refused a write because the generation precondition did not hold. It
// never escapes this package: Put translates it into a *ConflictError
// carrying the revision that is actually there.
var errGCSPrecondition = errors.New("gcs precondition failed")

// newGCSMemoryClient constructs the client OpenGCS uses. A package var
// so tests can substitute a fake without a build tag; production always
// gets the real ADC-backed client.
var newGCSMemoryClient = func(ctx context.Context) (gcsMemoryAPI, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("google cloud storage: %w — credentials resolve via Application Default Credentials; "+
			"run `gcloud auth application-default login`, or attach a workload identity / service account to the workload", err)
	}
	return &storageMemoryAPI{c: c}, nil
}

// OpenGCS opens a memory store over gs://bucket/prefix.
func OpenGCS(ctx context.Context, bucket, prefix string) (*GCSStore, error) {
	if bucket == "" {
		return nil, errors.New("gcs memory store needs a bucket")
	}
	api, err := newGCSMemoryClient(ctx)
	if err != nil {
		return nil, err
	}
	return &GCSStore{api: api, bucket: bucket, prefix: normalisePrefix(prefix), owned: true}, nil
}

// newGCSStoreWithAPI builds a store over an explicit API. Test-only
// entry point; production goes through OpenGCS.
func newGCSStoreWithAPI(api gcsMemoryAPI, bucket, prefix string) *GCSStore {
	return &GCSStore{api: api, bucket: bucket, prefix: normalisePrefix(prefix)}
}

// normalisePrefix trims leading slashes and guarantees a trailing one.
func normalisePrefix(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(p, "/") + "/"
}

// Close releases the underlying client, if this store owns one.
func (s *GCSStore) Close() error {
	if !s.owned || s.api == nil {
		return nil
	}
	err := s.api.Close()
	s.api = nil
	return err
}

// Describe implements Store.
func (s *GCSStore) Describe() string { return "gcs://" + s.bucket + "/" + s.prefix }

// Location implements Store.
func (s *GCSStore) Location(key string) string { return "gs://" + s.bucket + "/" + s.object(key) }

// object maps a store-relative key to its full object name.
func (s *GCSStore) object(key string) string { return s.prefix + key }

// Load implements Store.
func (s *GCSStore) Load(ctx context.Context) ([]Record, error) {
	objs, err := s.api.List(ctx, s.bucket, s.prefix)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", s.Describe(), err)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })

	out := make([]Record, 0, len(objs))
	for _, o := range objs {
		key, kerr := s.liveDocumentKey(o.Name)
		if kerr != nil {
			if !errors.Is(kerr, errNotAMemoryDocument) {
				fmt.Fprintf(os.Stderr, "meerkat: skipping memory object %q: %v\n", o.Name, kerr)
			}
			continue
		}
		// Read the exact generation the listing named, not "whatever is
		// current": a concurrent write during startup must not produce a
		// record whose bytes and version disagree.
		body, gen, rerr := s.api.Read(ctx, s.bucket, o.Name, o.Generation)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "meerkat: skipping memory %s: %v\n", s.Location(key), rerr)
			continue
		}
		if len(body) > maxDocumentBytes {
			fmt.Fprintf(os.Stderr, "meerkat: skipping memory %s: %d bytes, over the %d-byte limit\n",
				s.Location(key), len(body), maxDocumentBytes)
			continue
		}
		out = append(out, Record{Key: key, Body: body, Version: generationVersion(gen)})
	}
	return out, nil
}

// errNotAMemoryDocument marks an object under the store prefix that is
// not a live memory document at all — a staged proposal, a stray
// non-markdown file, the prefix placeholder. Distinguished from an
// unsafe key so that Load can warn about the second and stay silent
// about the first.
var errNotAMemoryDocument = errors.New("not a live memory document")

// liveDocumentKey maps a full object name to the store-relative key of a
// LIVE memory document.
//
// It is shared by Load and Fingerprint, and sharing it is the point: the
// fingerprint's contract is that it changes if and only if what Load
// returns would change (see Fingerprinter), and two independent copies
// of "which objects count" is exactly how that stops being true. A
// staged proposal excluded here is excluded from both — so writing one
// neither publishes it nor triggers a fleet-wide reload.
func (s *GCSStore) liveDocumentKey(name string) (string, error) {
	key := strings.TrimPrefix(name, s.prefix)
	if key == "" || !strings.HasSuffix(key, ".md") {
		return "", errNotAMemoryDocument
	}
	// Staged artifacts are excluded here exactly as they are in the local
	// backend: a pending proposal must never be loaded into a collection,
	// whichever backend it is sitting in.
	if Staged(key) {
		return "", errNotAMemoryDocument
	}
	if err := checkKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// Fingerprint implements Fingerprinter: a sha256 over the sorted
// (object name, generation) pairs of every LIVE document in the store.
//
// One list call, no reads. Every write to a live document assigns a new
// generation, and every create or delete changes the set, so the digest
// changes on exactly the events a reader needs to reload for — and on no
// others. It is the same construction internal/contentsource uses to key
// a prefix-mode content cache, for the same reason: a listing's
// generations are the cheapest honest summary object storage offers.
//
// Truncated to 32 hex characters. It is a change detector compared
// against a value this process itself recorded, not a security token:
// there is no second party to forge one, and the full digest would only
// make log lines longer.
func (s *GCSStore) Fingerprint(ctx context.Context) (string, error) {
	objs, err := s.api.List(ctx, s.bucket, s.prefix)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", s.Describe(), err)
	}
	live := make([]gcsObject, 0, len(objs))
	for _, o := range objs {
		if _, kerr := s.liveDocumentKey(o.Name); kerr != nil {
			continue
		}
		live = append(live, o)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })

	h := sha256.New()
	for _, o := range live {
		fmt.Fprintf(h, "%s\x00%d\n", o.Name, o.Generation)
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

// Stat implements Store.
func (s *GCSStore) Stat(ctx context.Context, key string) (Version, bool, error) {
	if err := checkKey(key); err != nil {
		return "", false, err
	}
	_, gen, err := s.api.Read(ctx, s.bucket, s.object(key), 0)
	if err != nil {
		if isNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", s.Location(key), err)
	}
	return generationVersion(gen), true, nil
}

// isNotExist reports whether err is GCS's "no such object".
func isNotExist(err error) bool {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// Put implements Store, translating the backend's precondition failure
// into a *ConflictError that names the revision now in place.
func (s *GCSStore) Put(ctx context.Context, key string, body []byte, pre Precondition) (Version, error) {
	if err := checkWrite(key, body, pre, true); err != nil {
		return "", err
	}
	ifGeneration := int64(0)
	if !pre.Absent {
		gen, err := parseGeneration(pre.Version)
		if err != nil {
			return "", err
		}
		ifGeneration = gen
	}
	gen, err := s.api.Write(ctx, s.bucket, s.object(key), body, ifGeneration)
	if errors.Is(err, errGCSPrecondition) {
		return "", &ConflictError{Key: key, Current: s.currentVersion(ctx, key)}
	}
	if err != nil {
		return "", fmt.Errorf("write %s: %w", s.Location(key), err)
	}
	return generationVersion(gen), nil
}

// Stage implements Store.
func (s *GCSStore) Stage(ctx context.Context, key string, body []byte) (string, error) {
	if err := checkWrite(key, body, Precondition{}, false); err != nil {
		return "", err
	}
	if !strings.HasPrefix(key, StagingPrefix+"/") {
		return "", fmt.Errorf("staged memory key %q must be under %s/", key, StagingPrefix)
	}
	if err := s.api.WriteUnconditional(ctx, s.bucket, s.object(key), body); err != nil {
		return "", fmt.Errorf("stage %s: %w", s.Location(key), err)
	}
	return s.Location(key), nil
}

// currentVersion re-reads key's generation after a precondition
// failure, best effort: an object deleted in the meantime yields an
// empty version, which ConflictError renders as "changed by someone
// else" rather than inventing a revision.
func (s *GCSStore) currentVersion(ctx context.Context, key string) Version {
	v, _, err := s.Stat(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

// generationVersion renders an object generation as a Version.
func generationVersion(gen int64) Version { return Version(strconv.FormatInt(gen, 10)) }

// parseGeneration reads a Version back as an object generation.
func parseGeneration(v Version) (int64, error) {
	gen, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil || gen <= 0 {
		return 0, fmt.Errorf("%q is not a version this memory store issued", string(v))
	}
	return gen, nil
}

// --- the real client -------------------------------------------------

type storageMemoryAPI struct{ c *storage.Client }

func (s *storageMemoryAPI) List(ctx context.Context, bucket, prefix string) ([]gcsObject, error) {
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
		if len(out) >= maxMemoryObjects {
			return nil, fmt.Errorf("memory prefix %q holds more than %d objects; refusing", prefix, maxMemoryObjects)
		}
		out = append(out, gcsObject{Name: a.Name, Generation: a.Generation})
	}
}

func (s *storageMemoryAPI) Read(ctx context.Context, bucket, object string, generation int64) ([]byte, int64, error) {
	obj := s.c.Bucket(bucket).Object(object)
	if generation > 0 {
		// Both the generation selector and the matching precondition, for
		// the same reason internal/contentsource's reader sets both: ask
		// for exactly those bytes, and fail rather than silently serve
		// something else if that cannot be honoured.
		obj = obj.Generation(generation).If(storage.Conditions{GenerationMatch: generation})
	}
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = r.Close() }()
	body, err := io.ReadAll(io.LimitReader(r, maxDocumentBytes+1))
	if err != nil {
		return nil, 0, err
	}
	gen := generation
	if gen == 0 {
		gen = r.Attrs.Generation
	}
	return body, gen, nil
}

func (s *storageMemoryAPI) Write(ctx context.Context, bucket, object string, body []byte, ifGeneration int64) (int64, error) {
	cond := storage.Conditions{DoesNotExist: true}
	if ifGeneration > 0 {
		cond = storage.Conditions{GenerationMatch: ifGeneration}
	}
	w := s.c.Bucket(bucket).Object(object).If(cond).NewWriter(ctx)
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return 0, translatePrecondition(err)
	}
	if err := w.Close(); err != nil {
		return 0, translatePrecondition(err)
	}
	return w.Attrs().Generation, nil
}

func (s *storageMemoryAPI) WriteUnconditional(ctx context.Context, bucket, object string, body []byte) error {
	w := s.c.Bucket(bucket).Object(object).NewWriter(ctx)
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (s *storageMemoryAPI) Close() error { return s.c.Close() }

// translatePrecondition maps GCS's precondition rejection onto the
// package-internal sentinel. 412 is the documented answer to a failed
// ifGenerationMatch; 404 arrives when the matched generation no longer
// exists at all, which is the same story from the caller's side.
func translatePrecondition(err error) error {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && (gerr.Code == http.StatusPreconditionFailed || gerr.Code == http.StatusNotFound) {
		return fmt.Errorf("%w: %v", errGCSPrecondition, err)
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("%w: %v", errGCSPrecondition, err)
	}
	return err
}

// maxMemoryObjects caps how many objects a memory prefix may hold. A
// misconfigured prefix ("" instead of "kb/memory/") can name an entire
// bucket, and Load fetches every object before the server begins
// serving; failing loudly at a sane bound beats a silent, unbounded
// startup. Mirrors internal/contentsource's maxGCSObjects in spirit.
const maxMemoryObjects = 20_000
