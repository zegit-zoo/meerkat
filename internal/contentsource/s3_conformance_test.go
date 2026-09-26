package contentsource

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zegit-zoo/meerkat/internal/s3api"
)

// s3_conformance_test.go talks to a REAL S3-compatible store and checks
// the properties the read side depends on: HeadObject and listings
// report ETags, a GetObject with a stale If-Match is refused rather
// than served, and listing pagination is followed. It is what keeps
// the provider matrix in docs/design/object-stores.md honest.
//
// Opt-in: set MEERKAT_TEST_S3_ENDPOINT (plus _BUCKET, _REGION,
// _PATH_STYLE, and AWS credentials in the environment); see
// scripts/garage-up.sh. Skipped otherwise, so `go test ./...` never
// needs a store.

type conformanceEnv struct {
	cfg      s3api.Config
	bucket   string
	provider string
	client   *s3.Client
}

func conformance(t *testing.T) conformanceEnv {
	t.Helper()
	endpoint := os.Getenv("MEERKAT_TEST_S3_ENDPOINT")
	bucket := os.Getenv("MEERKAT_TEST_S3_BUCKET")
	if endpoint == "" && bucket == "" {
		t.Skip("MEERKAT_TEST_S3_ENDPOINT/BUCKET not set; skipping S3 conformance (see scripts/garage-up.sh and scripts/versitygw-up.sh)")
	}
	if bucket == "" {
		t.Fatal("MEERKAT_TEST_S3_BUCKET is required when MEERKAT_TEST_S3_ENDPOINT is set")
	}
	cfg := s3api.Config{
		Endpoint:  endpoint,
		Region:    os.Getenv("MEERKAT_TEST_S3_REGION"),
		PathStyle: os.Getenv("MEERKAT_TEST_S3_PATH_STYLE") == "true",
	}
	client, err := s3api.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("s3api.New: %v", err)
	}
	return conformanceEnv{cfg: cfg, bucket: bucket, provider: os.Getenv("MEERKAT_TEST_S3_PROVIDER"), client: client}
}

// scratchPrefix returns a unique prefix for this test and deletes
// everything under it when the test ends.
func (e conformanceEnv) scratchPrefix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	_, _ = rand.Read(b[:])
	prefix := "meerkat-conformance/" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + hex.EncodeToString(b[:]) + "/"
	t.Cleanup(func() {
		ctx := context.Background()
		pager := s3.NewListObjectsV2Paginator(e.client, &s3.ListObjectsV2Input{Bucket: aws.String(e.bucket), Prefix: aws.String(prefix)})
		for pager.HasMorePages() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return
			}
			for _, o := range page.Contents {
				_, _ = e.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(e.bucket), Key: o.Key})
			}
		}
	})
	return prefix
}

func (e conformanceEnv) put(t *testing.T, key string, body []byte) string {
	t.Helper()
	out, err := e.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(e.bucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
	return s3api.CleanETag(out.ETag)
}

func (e conformanceEnv) source(prefix string) Source {
	return Source{Type: TypeS3, Bucket: e.bucket, Endpoint: e.cfg.Endpoint, Region: e.cfg.Region, PathStyle: e.cfg.PathStyle, Layout: defaultLayout()}
}

func isolateCache(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(base, "AppData", "Local"))
}

func TestS3Conformance_BundleETagAndIfMatch(t *testing.T) {
	e := conformance(t)
	isolateCache(t)
	prefix := e.scratchPrefix(t)
	key := prefix + "kb.tar.gz"
	etag := e.put(t, key, kbTarGz(t, map[string]string{"wiki/a.md": "# A\n"}))
	if etag == "" {
		t.Fatalf("%s: PutObject returned no ETag", e.provider)
	}
	src := e.source(prefix)
	src.Object = key

	dir, version, err := FetchS3(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchS3: %v", err)
	}
	if version != etag {
		t.Errorf("%s: fetched version %q, HeadObject ETag %q — they must agree", e.provider, version, etag)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "wiki", "a.md")); string(got) != "# A\n" {
		t.Errorf("extracted %q", got)
	}

	// Overwrite, then read with the OLD ETag pinned: the provider must
	// refuse (412), never serve the new bytes under the old name.
	etag2 := e.put(t, key, kbTarGz(t, map[string]string{"wiki/a.md": "# A2\n"}))
	if etag2 == etag {
		t.Fatalf("%s: overwrite with different content kept ETag %s", e.provider, etag)
	}
	client, err := newS3Client(context.Background(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := client.Open(context.Background(), e.bucket, key, etag)
	if err == nil {
		_ = rc.Close()
		t.Fatalf("%s: GetObject with a stale If-Match was SERVED — this provider does not enforce If-Match on reads; "+
			"record it in docs/design/object-stores.md and do not use it for type: s3", e.provider)
	}
	if !s3api.IsPreconditionFailed(err) {
		t.Errorf("%s: stale If-Match failed with %v, want a precondition failure", e.provider, err)
	}
	pinned := src
	pinned.ETag = etag
	isolateCache(t)
	if _, _, err := FetchS3(context.Background(), pinned); err == nil {
		t.Errorf("%s: a pinned, replaced bundle was fetched", e.provider)
	}
	if _, v, err := FetchS3(context.Background(), src); err != nil || v != etag2 {
		t.Errorf("unpinned fetch after overwrite = %q %v, want %q", v, err, etag2)
	}
}

func TestS3Conformance_PrefixListingAndFingerprint(t *testing.T) {
	e := conformance(t)
	isolateCache(t)
	prefix := e.scratchPrefix(t)
	e.put(t, prefix+"wiki/a.md", []byte("# A\n"))
	e.put(t, prefix+"wiki/sub/b.md", []byte("# B\n"))
	src := e.source(prefix)
	src.Prefix = prefix

	dir, version, err := FetchS3(context.Background(), src)
	if err != nil {
		t.Fatalf("FetchS3 prefix: %v", err)
	}
	for _, want := range []string{"wiki/a.md", "wiki/sub/b.md"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s missing: %v", want, err)
		}
	}
	probe, err := S3Version(context.Background(), src)
	if err != nil || probe != version {
		t.Errorf("S3Version = %q %v, want %q", probe, err, version)
	}
	e.put(t, prefix+"wiki/a.md", []byte("# A changed\n"))
	v2, err := S3Version(context.Background(), src)
	if err != nil || v2 == version {
		t.Errorf("%s: overwrite did not move the listing fingerprint (%q, %v)", e.provider, v2, err)
	}
}

func TestS3Conformance_ListingPagination(t *testing.T) {
	e := conformance(t)
	if testing.Short() {
		t.Skip("pagination needs >1000 objects; skipped under -short")
	}
	prefix := e.scratchPrefix(t)
	const n = 1005 // one page is 1000 keys; the adapter must follow the continuation token
	for i := 0; i < n; i++ {
		e.put(t, prefix+"wiki/p"+strings.Repeat("0", 4-len(itoa(i)))+itoa(i)+".md", []byte("x"))
	}
	client, err := newS3Client(context.Background(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := client.Objects(context.Background(), e.bucket, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != n {
		t.Errorf("%s: listed %d objects, want %d — pagination not followed", e.provider, len(objs), n)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
