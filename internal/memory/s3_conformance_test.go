package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zegit-zoo/meerkat/internal/s3api"
)

// s3_conformance_test.go talks to a REAL S3-compatible store and checks
// the one property the memory store's correctness rests on: whether the
// backend ENFORCES conditional writes. A provider that accepts a
// PutObject with a stale If-Match, or an If-None-Match: * against an
// existing key, turns optimistic locking into silent lost updates.
//
// The observed behaviour is compared against providerEnforcesWrites —
// the table docs/design/object-stores.md publishes — so a provider
// upgrade that changes the answer fails this test and forces the doc to
// move with it. An unknown provider is reported, not judged.
//
// Opt-in through MEERKAT_TEST_S3_ENDPOINT; see scripts/garage-up.sh.

// providerEnforcesWrites is what docs/design/object-stores.md claims
// per provider (MEERKAT_TEST_S3_PROVIDER) about If-None-Match: * and
// If-Match on PutObject.
var providerEnforcesWrites = map[string]bool{
	"aws":    true,
	"minio":  true,
	"garage": false, // Garage 2.4: enforces If-Match on GET, ignores both headers on PUT.
}

func conformanceSpec(t *testing.T) (*Spec, *s3.Client) {
	t.Helper()
	endpoint := os.Getenv("MEERKAT_TEST_S3_ENDPOINT")
	bucket := os.Getenv("MEERKAT_TEST_S3_BUCKET")
	if endpoint == "" && bucket == "" {
		t.Skip("MEERKAT_TEST_S3_ENDPOINT/BUCKET not set; skipping S3 conformance (see scripts/garage-up.sh)")
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	spec := &Spec{
		Type:      BackendS3,
		Bucket:    bucket,
		Prefix:    "meerkat-conformance/" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + hex.EncodeToString(b[:]) + "/",
		Endpoint:  endpoint,
		Region:    os.Getenv("MEERKAT_TEST_S3_REGION"),
		PathStyle: os.Getenv("MEERKAT_TEST_S3_PATH_STYLE") == "true",
		SSE:       os.Getenv("MEERKAT_TEST_S3_SSE"),
	}
	client, err := s3api.New(context.Background(), s3api.Config{Endpoint: spec.Endpoint, Region: spec.Region, PathStyle: spec.PathStyle})
	if err != nil {
		t.Fatalf("s3api.New: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(spec.Prefix)})
		for pager.HasMorePages() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return
			}
			for _, o := range page.Contents {
				_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key})
			}
		}
	})
	return spec, client
}

// TestS3Conformance_ConditionalWriteMatrix observes what the provider
// does with the two write preconditions and the read precondition, and
// checks the observation against the published table.
func TestS3Conformance_ConditionalWriteMatrix(t *testing.T) {
	spec, _ := conformanceSpec(t)
	provider := os.Getenv("MEERKAT_TEST_S3_PROVIDER")
	ctx := context.Background()
	api, err := newS3MemoryClient(ctx, s3api.Config{Endpoint: spec.Endpoint, Region: spec.Region, PathStyle: spec.PathStyle})
	if err != nil {
		t.Fatal(err)
	}
	key := spec.Prefix + "matrix.md"
	etag, err := api.Write(ctx, spec.Bucket, key, []byte("one"), true, "", spec.SSE)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if etag == "" {
		t.Fatalf("%s: PutObject returned no ETag", provider)
	}
	_, err = api.Write(ctx, spec.Bucket, key, []byte("two"), true, "", spec.SSE)
	ifNoneMatch := errors.Is(err, errS3Precondition)
	_, err = api.Write(ctx, spec.Bucket, key, []byte("three"), false, "0"+etag[1:], spec.SSE)
	ifMatchPut := errors.Is(err, errS3Precondition)
	_, _, err = api.Read(ctx, spec.Bucket, key, "0"+etag[1:])
	ifMatchGet := errors.Is(err, errS3Precondition)
	t.Logf("provider=%q  PUT If-None-Match:* enforced=%v  PUT If-Match enforced=%v  GET If-Match enforced=%v",
		provider, ifNoneMatch, ifMatchPut, ifMatchGet)

	if !ifMatchGet {
		t.Errorf("%s: GET If-Match is not enforced — type: s3 content sources cannot pin what they serve here", provider)
	}
	want, known := providerEnforcesWrites[provider]
	if !known {
		t.Logf("provider %q is not in providerEnforcesWrites; add its row to docs/design/object-stores.md", provider)
		return
	}
	if got := ifNoneMatch && ifMatchPut; got != want {
		t.Errorf("%s: conditional writes enforced=%v, but docs/design/object-stores.md says %v — update the table (and providerEnforcesWrites)",
			provider, got, want)
	}
	// The startup probe must agree with the observation.
	st := newS3StoreWithAPI(api, spec.Bucket, spec.Prefix, spec.SSE)
	perr := st.verifyConditionalWrites(ctx)
	if want && perr != nil {
		t.Errorf("probe refused an enforcing provider: %v", perr)
	}
	if !want && !errors.Is(perr, ErrConditionalWritesNotEnforced) {
		t.Errorf("probe accepted a non-enforcing provider: %v", perr)
	}
}

// TestS3Conformance_StoreFlow runs the store end to end in the mode the
// provider supports: backend-enforced where the table says so,
// single_writer otherwise — and checks that OpenS3 refuses the other.
func TestS3Conformance_StoreFlow(t *testing.T) {
	spec, _ := conformanceSpec(t)
	provider := os.Getenv("MEERKAT_TEST_S3_PROVIDER")
	ctx := context.Background()
	enforces, known := providerEnforcesWrites[provider]
	if !known {
		t.Skipf("provider %q not in providerEnforcesWrites; run the matrix test first", provider)
	}

	if !enforces {
		if _, err := OpenS3(ctx, spec); !errors.Is(err, ErrConditionalWritesNotEnforced) {
			t.Fatalf("%s: OpenS3 without single_writer must refuse, got %v", provider, err)
		}
		spec.SingleWriter = true
	}
	st, err := OpenS3(ctx, spec)
	if err != nil {
		t.Fatalf("OpenS3: %v", err)
	}

	v1, err := st.Put(ctx, "team/note.md", []byte("one"), CreateOnly())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Put(ctx, "team/note.md", []byte("clobber"), CreateOnly()); !errors.Is(err, ErrConflict) {
		t.Fatalf("create-only over an existing key: %v, want ErrConflict", err)
	}
	v2, err := st.Put(ctx, "team/note.md", []byte("two"), UpdateFrom(v1))
	if err != nil {
		t.Fatalf("update from current: %v", err)
	}
	if v2 == v1 {
		t.Errorf("%s: update with new content kept ETag %s", provider, v1)
	}
	_, err = st.Put(ctx, "team/note.md", []byte("stale"), UpdateFrom(v1))
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("update from a stale version: %v, want *ConflictError", err)
	}
	if conflict.Current != v2 {
		t.Errorf("conflict reports current %q, want %q", conflict.Current, v2)
	}
	body, _, _ := st.api.Read(ctx, spec.Bucket, spec.Prefix+"team/note.md", "")
	if string(body) != "two" {
		t.Errorf("a refused stale write changed the object to %q", body)
	}

	got, ok, err := st.Stat(ctx, "team/note.md")
	if err != nil || !ok || got != v2 {
		t.Errorf("Stat = %q ok=%v err=%v, want %q", got, ok, err, v2)
	}
	recs, err := st.Load(ctx)
	if err != nil || len(recs) != 1 || recs[0].Version != v2 || string(recs[0].Body) != "two" {
		t.Errorf("Load = %+v %v", recs, err)
	}
	fp1, _ := st.Fingerprint(ctx)
	if _, err := st.Stage(ctx, StagingPrefix+"/team/ns/p.md", []byte("pending")); err != nil {
		t.Fatal(err)
	}
	fp2, _ := st.Fingerprint(ctx)
	if fp1 != fp2 {
		t.Error("a staged write moved the fingerprint")
	}
	if _, err := st.Put(ctx, "team/other.md", []byte("x"), CreateOnly()); err != nil {
		t.Fatal(err)
	}
	if fp3, _ := st.Fingerprint(ctx); fp3 == fp2 {
		t.Error("a live write did not move the fingerprint")
	}
}
