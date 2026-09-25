package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zegit-zoo/meerkat/internal/s3api"
)

// S3Store is a memory store backed by a prefix in an S3-compatible
// bucket: AWS S3, or a self-hosted store such as Garage or Versity
// Gateway.
//
// # Optimistic locking, for real
//
// Like GCSStore, and unlike the local backend, the compare-and-swap is
// pushed down to the backend as a conditional write:
//
//	create  ->  If-None-Match: *        (succeeds only if nothing is there)
//	update  ->  If-Match: "<etag>"      (succeeds only against that revision)
//
// The backend evaluates both server-side and fails the write with 412
// otherwise, so several meerkat replicas can share one memory store
// and never lose a write. The version a caller holds is the object's
// ETag — opaque, never interpreted, only compared and sent back.
//
// # Not every S3 enforces that
//
// A provider that accepts a PutObject with a stale If-Match, or an
// If-None-Match: * against an existing key, turns a refused stale write
// into a silent lost update — the exact failure this backend exists to
// rule out. Garage (2.4) is one: it enforces If-Match on reads and
// ignores both headers on writes. So OpenS3 PROVES the property before
// trusting it: it writes a probe object under the staging prefix,
// re-writes it with preconditions that must fail, and deletes it. A
// provider that accepts either write is refused with
// ErrConditionalWritesNotEnforced.
//
// The escape hatch is `single_writer: true`: the operator asserts that
// exactly one meerkat process writes this store, and the store then
// enforces preconditions itself — a mutex plus a HeadObject compare —
// which is precisely the guarantee the local backend gives and no more.
// The probe is skipped, and the conditional headers are still sent (a
// provider that starts honouring them costs nothing).
// docs/design/object-stores.md records what each provider does, and the
// opt-in conformance test (s3_conformance_test.go) keeps that honest.
//
// Authentication is the AWS default chain, exactly as
// internal/contentsource's read-side S3 support: there is no field
// anywhere in the schema for a static access key.
type S3Store struct {
	api    s3MemoryAPI
	bucket string
	// prefix always ends in "/" (or is empty), so key joining is a
	// concatenation with no separator arithmetic at the call sites.
	prefix string
	sse    string
	// singleWriter enforces preconditions in-process instead of
	// trusting the backend to. mu serialises Put in that mode.
	singleWriter bool
	mu           sync.Mutex
}

// ErrConditionalWritesNotEnforced is returned by OpenS3 when the probe
// finds the backend accepting writes it should have refused.
var ErrConditionalWritesNotEnforced = errors.New("s3 backend does not enforce conditional writes")

// s3MemoryObject is the object metadata this package needs. A small
// local struct so the seam below can be faked without SDK types.
type s3MemoryObject struct {
	Key  string
	ETag string
}

// s3MemoryAPI is the slice of the S3 API this package uses. It exists
// as an interface for one reason: it is the test seam. ETags cross it
// unquoted in both directions.
type s3MemoryAPI interface {
	// List returns every object under prefix.
	List(ctx context.Context, bucket, prefix string) ([]s3MemoryObject, error)
	// Read returns the body and ETag of key; when ifMatch is set it is
	// a precondition and a changed object fails with errS3Precondition.
	Read(ctx context.Context, bucket, key, ifMatch string) ([]byte, string, error)
	// Head returns the ETag of key.
	Head(ctx context.Context, bucket, key string) (string, error)
	// Write stores body at key and returns its new ETag. createOnly
	// sends If-None-Match: *; otherwise ifMatch is sent as If-Match. A
	// refused precondition is errS3Precondition.
	Write(ctx context.Context, bucket, key string, body []byte, createOnly bool, ifMatch, sse string) (string, error)
	// WriteUnconditional stores body at key with no precondition.
	WriteUnconditional(ctx context.Context, bucket, key string, body []byte, sse string) error
	// Delete removes key; a missing key is not an error.
	Delete(ctx context.Context, bucket, key string) error
}

// errS3Precondition is what the API returns when the backend refused a
// conditional write or read. Internal: the store translates it into a
// *ConflictError.
var errS3Precondition = errors.New("s3 precondition failed")

// errS3NotExist is what the API returns for a missing key.
var errS3NotExist = errors.New("s3 object does not exist")

// newS3MemoryClient constructs the client OpenS3 uses. A package var so
// tests can substitute a fake; production always gets the real
// default-chain client.
var newS3MemoryClient = func(ctx context.Context, cfg s3api.Config) (s3MemoryAPI, error) {
	c, err := s3api.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &s3StoreAPI{c: c}, nil
}

// OpenS3 opens a memory store over s3://bucket/prefix at the given
// endpoint (empty for AWS), and — unless the spec says single_writer —
// proves the backend enforces conditional writes before returning it.
func OpenS3(ctx context.Context, spec *Spec) (*S3Store, error) {
	if spec == nil || spec.Bucket == "" {
		return nil, errors.New("s3 memory store needs a bucket")
	}
	api, err := newS3MemoryClient(ctx, s3api.Config{Endpoint: spec.Endpoint, Region: spec.Region, PathStyle: spec.PathStyle})
	if err != nil {
		return nil, err
	}
	s := newS3StoreWithAPI(api, spec.Bucket, spec.Prefix, spec.SSE)
	s.singleWriter = spec.SingleWriter
	if !s.singleWriter {
		if err := s.verifyConditionalWrites(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// newS3StoreWithAPI wires a store to an already-built API — the test
// entry point; production goes through OpenS3.
func newS3StoreWithAPI(api s3MemoryAPI, bucket, prefix, sse string) *S3Store {
	return &S3Store{api: api, bucket: bucket, prefix: normalisePrefix(prefix), sse: sse}
}

// verifyConditionalWrites writes a probe document under the staging
// prefix (never loaded, never fingerprinted — see liveDocumentKey),
// then issues the two writes a conforming backend must refuse, and
// deletes the probe. Any accepted write means the backend would also
// accept a stale memory update, and the store refuses to open.
//
// The probe costs three PUTs and a DELETE per process start. The key
// carries a random suffix and the delete is best-effort, so a policy
// without s3:DeleteObject leaves one small object under _staging/_probe/
// per start and never wedges startup; it is skipped entirely under
// single_writer, where it would prove nothing.
func (s *S3Store) verifyConditionalWrites(ctx context.Context) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("probe key: %w", err)
	}
	key := s.object(StagingPrefix + "/_probe/" + hex.EncodeToString(b[:]) + ".md")
	defer func() { _ = s.api.Delete(ctx, s.bucket, key) }()

	etag, err := s.api.Write(ctx, s.bucket, key, []byte("meerkat conditional-write probe\n"), true, "", s.sse)
	if err != nil {
		return fmt.Errorf("probe write to %s: %w", s.Describe(), err)
	}
	// If-None-Match: * against a key that now exists.
	if _, err := s.api.Write(ctx, s.bucket, key, []byte("probe 2\n"), true, "", s.sse); !errors.Is(err, errS3Precondition) {
		if err != nil {
			return fmt.Errorf("probe create-only rewrite failed unexpectedly: %w", err)
		}
		return fmt.Errorf("%w: %s accepted a create-only write (If-None-Match: *) over an existing object — "+
			"a shared memory store would lose updates here (Garage 2.4 is known to do this: it honours If-Match on GET but ignores "+
			"If-None-Match: * and a stale If-Match on PUT); set single_writer: true if exactly one meerkat process writes this store, "+
			"or use a backend that enforces conditional writes — AWS S3, Versity Gateway, GCS (see docs/design/object-stores.md)",
			ErrConditionalWritesNotEnforced, s.Describe())
	}
	// If-Match against an ETag that is not current.
	stale := "0" + etag[1:]
	if etag == "" {
		stale = "deadbeef"
	}
	if _, err := s.api.Write(ctx, s.bucket, key, []byte("probe 3\n"), false, stale, s.sse); !errors.Is(err, errS3Precondition) {
		if err != nil {
			return fmt.Errorf("probe stale update failed unexpectedly: %w", err)
		}
		return fmt.Errorf("%w: %s accepted an update (If-Match) against a stale ETag — "+
			"a shared memory store would lose updates here (Garage 2.4 is known to do this: it honours If-Match on GET but ignores "+
			"If-None-Match: * and a stale If-Match on PUT); set single_writer: true if exactly one meerkat process writes this store, "+
			"or use a backend that enforces conditional writes — AWS S3, Versity Gateway, GCS (see docs/design/object-stores.md)",
			ErrConditionalWritesNotEnforced, s.Describe())
	}
	return nil
}

// Describe implements Store.
func (s *S3Store) Describe() string { return "s3://" + s.bucket + "/" + s.prefix }

// Location implements Store.
func (s *S3Store) Location(key string) string { return "s3://" + s.bucket + "/" + s.object(key) }

func (s *S3Store) object(key string) string { return s.prefix + key }

// Load implements Store: every live memory document under the prefix.
// Each read is conditional on the ETag the listing reported, so a
// document overwritten between the listing and its read is skipped
// (and picked up by the next reconciliation) rather than loaded under
// a version it no longer has.
func (s *S3Store) Load(ctx context.Context) ([]Record, error) {
	objs, err := s.api.List(ctx, s.bucket, s.prefix)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", s.Describe(), err)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })

	out := make([]Record, 0, len(objs))
	for _, o := range objs {
		key, kerr := s.liveDocumentKey(o.Key)
		if kerr != nil {
			if !errors.Is(kerr, errNotAMemoryDocument) {
				fmt.Fprintf(os.Stderr, "meerkat: skipping memory object %q: %v\n", o.Key, kerr)
			}
			continue
		}
		body, etag, rerr := s.api.Read(ctx, s.bucket, o.Key, o.ETag)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "meerkat: skipping memory %s: %v\n", s.Location(key), rerr)
			continue
		}
		if len(body) > maxDocumentBytes {
			fmt.Fprintf(os.Stderr, "meerkat: skipping memory %s: %d bytes, over the %d-byte limit\n",
				s.Location(key), len(body), maxDocumentBytes)
			continue
		}
		out = append(out, Record{Key: key, Body: body, Version: etagVersion(etag)})
	}
	return out, nil
}

// liveDocumentKey maps an object key to a memory key, or reports why
// the object is not a live document (a staged one, a non-.md object).
func (s *S3Store) liveDocumentKey(name string) (string, error) {
	key := strings.TrimPrefix(name, s.prefix)
	if key == "" || !strings.HasSuffix(key, ".md") {
		return "", errNotAMemoryDocument
	}
	if Staged(key) {
		return "", errNotAMemoryDocument
	}
	if err := checkKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// Fingerprint implements Fingerprinter: a hash over the sorted (key,
// ETag) pairs of every live document — one listing, no reads. Any
// create, overwrite or delete under the prefix changes it.
func (s *S3Store) Fingerprint(ctx context.Context) (string, error) {
	objs, err := s.api.List(ctx, s.bucket, s.prefix)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", s.Describe(), err)
	}
	live := make([]s3MemoryObject, 0, len(objs))
	for _, o := range objs {
		if _, kerr := s.liveDocumentKey(o.Key); kerr != nil {
			continue
		}
		live = append(live, o)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Key < live[j].Key })

	h := sha256.New()
	for _, o := range live {
		fmt.Fprintf(h, "%s\x00%s\n", o.Key, o.ETag)
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

// Stat implements Store.
func (s *S3Store) Stat(ctx context.Context, key string) (Version, bool, error) {
	if err := checkKey(key); err != nil {
		return "", false, err
	}
	etag, err := s.api.Head(ctx, s.bucket, s.object(key))
	if err != nil {
		if errors.Is(err, errS3NotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat %s: %w", s.Location(key), err)
	}
	return etagVersion(etag), true, nil
}

// Put implements Store with a backend-enforced precondition — or, in
// single-writer mode, one this process enforces under its own lock.
func (s *S3Store) Put(ctx context.Context, key string, body []byte, pre Precondition) (Version, error) {
	if err := checkWrite(key, body, pre, true); err != nil {
		return "", err
	}
	ifMatch := ""
	if !pre.Absent {
		etag, err := parseETagVersion(pre.Version)
		if err != nil {
			return "", err
		}
		ifMatch = etag
	}
	if s.singleWriter {
		s.mu.Lock()
		defer s.mu.Unlock()
		current, exists, err := s.Stat(ctx, key)
		if err != nil {
			return "", err
		}
		if (pre.Absent && exists) || (!pre.Absent && (!exists || string(current) != ifMatch)) {
			return "", &ConflictError{Key: key, Current: current}
		}
	}
	etag, err := s.api.Write(ctx, s.bucket, s.object(key), body, pre.Absent, ifMatch, s.sse)
	if errors.Is(err, errS3Precondition) {
		return "", &ConflictError{Key: key, Current: s.currentVersion(ctx, key)}
	}
	if err != nil {
		return "", fmt.Errorf("write %s: %w", s.Location(key), err)
	}
	if etag == "" {
		return "", fmt.Errorf("write %s: backend returned no ETag", s.Location(key))
	}
	return etagVersion(etag), nil
}

// Stage implements Store: an unconditional write under the staging
// prefix, where nothing is ever updated in place.
func (s *S3Store) Stage(ctx context.Context, key string, body []byte) (string, error) {
	if err := checkWrite(key, body, Precondition{}, false); err != nil {
		return "", err
	}
	if !strings.HasPrefix(key, StagingPrefix+"/") {
		return "", fmt.Errorf("staged memory key %q must be under %s/", key, StagingPrefix)
	}
	if err := s.api.WriteUnconditional(ctx, s.bucket, s.object(key), body, s.sse); err != nil {
		return "", fmt.Errorf("stage %s: %w", s.Location(key), err)
	}
	return s.Location(key), nil
}

func (s *S3Store) currentVersion(ctx context.Context, key string) Version {
	v, _, err := s.Stat(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

func etagVersion(etag string) Version { return Version(strings.Trim(etag, `"`)) }

func parseETagVersion(v Version) (string, error) {
	etag := strings.Trim(string(v), `"`)
	if etag == "" || strings.ContainsAny(etag, " \t\r\n") {
		return "", fmt.Errorf("%q is not a version this memory store issued", string(v))
	}
	return etag, nil
}

// --- the real API ------------------------------------------------------

// s3StoreAPI is the real s3MemoryAPI over the AWS SDK.
type s3StoreAPI struct{ c *s3.Client }

func (a *s3StoreAPI) List(ctx context.Context, bucket, prefix string) ([]s3MemoryObject, error) {
	var out []s3MemoryObject
	pager := s3.NewListObjectsV2Paginator(a.c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			if len(out) >= maxMemoryObjects {
				return nil, fmt.Errorf("memory prefix %q holds more than %d objects; refusing", prefix, maxMemoryObjects)
			}
			out = append(out, s3MemoryObject{Key: aws.ToString(o.Key), ETag: s3api.CleanETag(o.ETag)})
		}
	}
	return out, nil
}

func (a *s3StoreAPI) Read(ctx context.Context, bucket, key, ifMatch string) ([]byte, string, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if ifMatch != "" {
		in.IfMatch = aws.String(s3api.QuoteETag(ifMatch))
	}
	out, err := a.c.GetObject(ctx, in)
	if err != nil {
		return nil, "", translateS3Error(err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(out.Body, maxDocumentBytes+1))
	if err != nil {
		return nil, "", err
	}
	return body, s3api.CleanETag(out.ETag), nil
}

func (a *s3StoreAPI) Head(ctx context.Context, bucket, key string) (string, error) {
	out, err := a.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return "", translateS3Error(err)
	}
	return s3api.CleanETag(out.ETag), nil
}

func (a *s3StoreAPI) Write(ctx context.Context, bucket, key string, body []byte, createOnly bool, ifMatch, sse string) (string, error) {
	in := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("text/markdown; charset=utf-8"),
	}
	if createOnly {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = aws.String(s3api.QuoteETag(ifMatch))
	}
	if sse != "" {
		in.ServerSideEncryption = types.ServerSideEncryption(sse)
	}
	out, err := a.c.PutObject(ctx, in)
	if err != nil {
		return "", translateS3Error(err)
	}
	return s3api.CleanETag(out.ETag), nil
}

func (a *s3StoreAPI) Delete(ctx context.Context, bucket, key string) error {
	_, err := a.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil && !s3api.IsNotFound(err) {
		return err
	}
	return nil
}

func (a *s3StoreAPI) WriteUnconditional(ctx context.Context, bucket, key string, body []byte, sse string) error {
	in := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("text/markdown; charset=utf-8"),
	}
	if sse != "" {
		in.ServerSideEncryption = types.ServerSideEncryption(sse)
	}
	_, err := a.c.PutObject(ctx, in)
	return err
}

// translateS3Error maps the SDK's error shapes onto the two sentinels
// the store reasons about; anything else passes through unchanged.
func translateS3Error(err error) error {
	switch {
	case s3api.IsPreconditionFailed(err):
		return fmt.Errorf("%w: %v", errS3Precondition, err)
	case s3api.IsNotFound(err):
		return fmt.Errorf("%w: %v", errS3NotExist, err)
	}
	return err
}
