//go:build ignore

// Command s3mkbucket creates one bucket in an S3-compatible store,
// idempotently: an existing bucket this key already owns is success.
//
// It exists because Versity Gateway ships no bucket CLI the way MinIO
// bundled mc and Garage bundles /garage, so scripts/versitygw-up.sh has
// nothing to shell into. Rather than add a tool to the image, reuse the
// SDK client meerkat itself builds (internal/s3api) — the conformance
// bucket is then created through exactly the code path under test.
//
// Credentials come from the AWS default chain (AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY), like every other S3 caller in this repo.
//
// Run via: go run scripts/s3mkbucket.go -endpoint http://127.0.0.1:7070 -bucket meerkat-conformance
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zegit-zoo/meerkat/internal/s3api"
)

func main() {
	endpoint := flag.String("endpoint", "", "base URL of the S3 endpoint (required)")
	bucket := flag.String("bucket", "", "bucket to create (required)")
	region := flag.String("region", "us-east-1", "signing region")
	flag.Parse()
	if *endpoint == "" || *bucket == "" {
		fmt.Fprintln(os.Stderr, "s3mkbucket: -endpoint and -bucket are required")
		os.Exit(2)
	}

	ctx := context.Background()
	client, err := s3api.New(ctx, s3api.Config{Endpoint: *endpoint, Region: *region, PathStyle: true})
	if err != nil {
		fail(err)
	}
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(*bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		fail(fmt.Errorf("create bucket %q at %s: %w", *bucket, *endpoint, err))
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "s3mkbucket: %v\n", err)
	os.Exit(1)
}
