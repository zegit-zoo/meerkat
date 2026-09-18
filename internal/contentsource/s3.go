package contentsource

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zegit-zoo/meerkat/internal/s3api"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// s3.go implements type: s3 — an S3-compatible bucket (AWS S3, Garage,
// MinIO, ...) as a runtime content source, in the same two modes as
// type: gcs:
//
//   - object: a single .tar.gz bundle, keyed by the object's ETag. S3
//     has no generation counter; the ETag is the provider's token for
//     "these bytes" (an MD5 for a single-part upload, a hash of part
//     hashes for a multipart one, something else again under SSE-KMS).
//     meerkat treats it as OPAQUE: it is compared, cached under, and
//     sent back as If-Match so the fetch fails rather than serving a
//     newer object that replaced it mid-fetch. It is never assumed to
//     be a content hash — pin sha256: if you want one verified.
//   - prefix: an object prefix served as a directory tree, cached by a
//     fingerprint over every listed object's (key, ETag, size).
//
// Addressing: Garage and most self-hosted stores need path-style
// (endpoint/bucket/key); AWS uses virtual-host style. `path_style:
// true` selects the former. `endpoint:` is required for anything that
// is not AWS. `region:` is the signing region — Garage validates it
// against its s3_region — and falls back to the AWS default chain.
//
// Credentials are the AWS default chain (environment, shared config,
// IRSA / web identity, instance metadata) and nothing else, mirroring
// the ADC-only rule for GCS: there is no field for a static key.
//
// Provider differences (conditional writes, multipart ETags, listing
// pagination, checksums) are recorded in docs/design/object-stores.md
// and exercised by the opt-in conformance tests.

// s3Kind parametrises the shared object-store paths for S3.
var s3Kind = storeKind{
	scheme:        "s3",
	span:          telemetry.SpanS3,
	opKey:         telemetry.KeyS3Operation,
	objectType:    telemetry.SourceS3Object,
	prefixType:    telemetry.SourceS3Prefix,
	newClient:     func(ctx context.Context, src Source) (objectStore, error) { return newS3Client(ctx, src.s3Config()) },
	pinnedVersion: func(src Source) string { return s3api.CleanETag(&src.ETag) },
	cacheKey: func(src Source) string {
		mode, target := "object", src.Object
		if src.Object == "" {
			mode, target = "prefix", src.Prefix
		}
		return src.Endpoint + "\x00" + src.Bucket + "\x00" + mode + "\x00" + target
	},
	fingerprintSize: true,
	validate:        func(src Source, p string) error { return src.validateS3(p) },
}

// s3Config is the client configuration a source contributes.
func (s Source) s3Config() s3api.Config {
	return s3api.Config{Endpoint: s.Endpoint, Region: s.Region, PathStyle: s.PathStyle}
}

// newS3Client constructs the client FetchS3 uses. A package var so
// tests can substitute a fake; production always gets the real
// default-chain client.
var newS3Client = func(ctx context.Context, cfg s3api.Config) (objectStore, error) {
	c, err := s3api.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return s3ObjectStore{c: c}, nil
}

// s3ObjectStore is the real objectStore for S3. Versions are ETags
// without their quotes.
type s3ObjectStore struct{ c *s3.Client }

func (s s3ObjectStore) Attrs(ctx context.Context, bucket, object string) (storedObject, error) {
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(object)})
	if err != nil {
		return storedObject{}, err
	}
	return storedObject{Name: object, Version: s3api.CleanETag(out.ETag), Size: aws.ToInt64(out.ContentLength)}, nil
}

func (s s3ObjectStore) Objects(ctx context.Context, bucket, prefix string) ([]storedObject, error) {
	var out []storedObject
	pager := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			if len(out) >= maxStoreObjects {
				return nil, fmt.Errorf("prefix %q matches more than %d objects; refusing (narrow the prefix)", prefix, maxStoreObjects)
			}
			out = append(out, storedObject{Name: aws.ToString(o.Key), Version: s3api.CleanETag(o.ETag), Size: aws.ToInt64(o.Size)})
		}
	}
	return out, nil
}

func (s s3ObjectStore) Open(ctx context.Context, bucket, object, version string) (io.ReadCloser, error) {
	if strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("refusing to read %q without a version token", object)
	}
	// SECURITY / correctness: If-Match makes the read a precondition the
	// backend enforces. If the object was overwritten between the
	// listing and this read, the request fails with 412 instead of
	// serving bytes that do not belong to the cache entry they would be
	// written into. This is the S3 spelling of GCS's ifGenerationMatch.
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{
		Bucket:  aws.String(bucket),
		Key:     aws.String(object),
		IfMatch: aws.String(s3api.QuoteETag(version)),
	})
	if err != nil {
		if s3api.IsPreconditionFailed(err) {
			return nil, fmt.Errorf("etag %s of %q no longer current (precondition failed): %w", version, object, err)
		}
		return nil, err
	}
	return out.Body, nil
}

// Close releases nothing: the SDK client holds no connection state
// that outlives its transport's idle pool.
func (s s3ObjectStore) Close() error { return nil }

// S3Provenance formats the kb_source string reported for a type: s3
// source: "s3://<bucket>/<object>@<etag>" for a bundle, or
// "s3://<bucket>/<prefix>*@<fingerprint>" for a prefix tree. See
// ObjectProvenance.
func S3Provenance(src Source, version string) string { return s3Kind.provenance(src, version) }

// FetchS3 resolves a type: s3 source to a local content-repo-layout
// directory ready for kbdir.ConfigureLayout/FSLayout, and returns the
// version token identifying exactly what was served (see S3Provenance).
func FetchS3(ctx context.Context, src Source) (dir, version string, err error) {
	if src.Type != TypeS3 {
		return "", "", fmt.Errorf("FetchS3: type %q is not %q", src.Type, TypeS3)
	}
	return fetchStore(ctx, src, s3Kind)
}

// S3Version returns the version token a type: s3 source resolves to
// right now — the object's current ETag, or the fingerprint over the
// prefix listing — using metadata calls only. See ObjectVersion.
func S3Version(ctx context.Context, src Source) (string, error) {
	if src.Type != TypeS3 {
		return "", fmt.Errorf("S3Version: type %q is not %q", src.Type, TypeS3)
	}
	return storeVersion(ctx, src, s3Kind)
}

// validateS3 checks the shape of a type: s3 source.
func (s Source) validateS3(p string) error {
	if s.Bucket == "" {
		return fmt.Errorf("%s.bucket is required for type: s3", p)
	}
	if strings.Contains(s.Bucket, "/") {
		return fmt.Errorf("%s.bucket must be a bucket name, not a path or s3:// URL, got %q", p, s.Bucket)
	}
	switch {
	case s.Object == "" && s.Prefix == "":
		return fmt.Errorf("%s: type: s3 needs either object: (a .tar.gz bundle) or prefix: (an object prefix served as a directory tree)", p)
	case s.Object != "" && s.Prefix != "":
		return fmt.Errorf("%s: type: s3 takes object: or prefix:, not both — object: fetches one .tar.gz bundle, prefix: serves an object prefix as a directory tree", p)
	}
	if s.Endpoint != "" {
		if !strings.HasPrefix(s.Endpoint, "https://") && !strings.HasPrefix(s.Endpoint, "http://") {
			return fmt.Errorf("%s.endpoint must be an http(s):// URL, got %q", p, s.Endpoint)
		}
	}
	if s.ETag != "" {
		if s.Object == "" {
			return fmt.Errorf("%s.etag pins one bundle object, so it applies to object: and not to a prefix: tree", p)
		}
		if strings.ContainsAny(s.ETag, " \t\n") {
			return fmt.Errorf("%s.etag must be a bare ETag value, got %q", p, s.ETag)
		}
	}
	if s.Generation != 0 {
		return fmt.Errorf("%s.generation is a GCS object generation and does not apply to type: s3 — pin an S3 bundle with an etag", p)
	}
	if s.SHA256 != "" && !isHex64(s.SHA256) {
		return fmt.Errorf("%s.sha256 must be 64 hex characters (a sha256 digest), got %q (%d chars)", p, s.SHA256, len(s.SHA256))
	}
	if s.Prefix != "" && s.SHA256 != "" {
		return fmt.Errorf("%s.sha256 applies to object: (one archive's bytes), not to prefix: (many objects)", p)
	}
	return nil
}
