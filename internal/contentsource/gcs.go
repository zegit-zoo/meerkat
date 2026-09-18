package contentsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

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
// The fetch, probe and cache logic is the provider-neutral code in
// objectstore.go; this file is the GCS adapter (storageAPI) plus the
// constants that parametrise it (gcsKind).
//
// Authentication is Application Default Credentials (and therefore
// Workload Identity Federation, GKE/Cloud Run metadata, an impersonated
// principal, or a developer's `gcloud auth application-default login`),
// resolved by the Google client library from the ambient environment.
// meerkat never reads, stores, or accepts a static service-account key:
// the schema has no field to name one (see Source.Bucket's comment).

// gcsKind parametrises the shared object-store paths for GCS.
var gcsKind = storeKind{
	scheme:     "gcs",
	span:       telemetry.SpanGCS,
	opKey:      telemetry.KeyGCSOperation,
	objectType: telemetry.SourceGCSObject,
	prefixType: telemetry.SourceGCSPrefix,
	newClient:  func(ctx context.Context, _ Source) (objectStore, error) { return newGCSClient(ctx) },
	pinnedVersion: func(src Source) string {
		if src.Generation > 0 {
			return strconv.FormatInt(src.Generation, 10)
		}
		return ""
	},
	cacheKey: func(src Source) string {
		mode, target := "object", src.Object
		if src.Object == "" {
			mode, target = "prefix", src.Prefix
		}
		return src.Bucket + "\x00" + mode + "\x00" + target
	},
	fingerprintSize: false,
	validate:        func(src Source, p string) error { return src.validateGCS(p) },
}

// newGCSClient constructs the client FetchGCS uses. A package var so
// tests can substitute a fake without a build tag or an exported
// test-only knob; production always gets the real ADC-backed client.
var newGCSClient = func(ctx context.Context) (objectStore, error) { return newStorageAPI(ctx) }

// storageAPI is the real objectStore for GCS, over
// cloud.google.com/go/storage. Versions are decimal generations.
type storageAPI struct{ c *storage.Client }

func newStorageAPI(ctx context.Context) (objectStore, error) {
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

func (s storageAPI) Attrs(ctx context.Context, bucket, object string) (storedObject, error) {
	a, err := s.c.Bucket(bucket).Object(object).Attrs(ctx)
	if err != nil {
		return storedObject{}, err
	}
	return storedObject{Name: a.Name, Version: strconv.FormatInt(a.Generation, 10), Size: a.Size}, nil
}

func (s storageAPI) Objects(ctx context.Context, bucket, prefix string) ([]storedObject, error) {
	it := s.c.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var out []storedObject
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(out) >= maxStoreObjects {
			return nil, fmt.Errorf("prefix %q matches more than %d objects; refusing (narrow the prefix)", prefix, maxStoreObjects)
		}
		out = append(out, storedObject{Name: a.Name, Version: strconv.FormatInt(a.Generation, 10), Size: a.Size})
	}
}

func (s storageAPI) Open(ctx context.Context, bucket, object, version string) (io.ReadCloser, error) {
	generation, err := strconv.ParseInt(version, 10, 64)
	if err != nil || generation <= 0 {
		return nil, fmt.Errorf("%q is not a GCS generation", version)
	}
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

// GCSProvenance formats the kb_source string reported for a type: gcs
// source: "gcs://<bucket>/<object>@<generation>" for a bundle, or
// "gcs://<bucket>/<prefix>*@<fingerprint>" for a prefix tree. See
// ObjectProvenance.
func GCSProvenance(src Source, version string) string { return gcsKind.provenance(src, version) }

// FetchGCS resolves a type: gcs source to a local content-repo-layout
// directory ready for kbdir.ConfigureLayout/FSLayout, and returns the
// version token identifying exactly what was served (see GCSProvenance).
func FetchGCS(ctx context.Context, src Source) (dir, version string, err error) {
	if src.Type != TypeGCS {
		return "", "", fmt.Errorf("FetchGCS: type %q is not %q", src.Type, TypeGCS)
	}
	return fetchStore(ctx, src, gcsKind)
}
