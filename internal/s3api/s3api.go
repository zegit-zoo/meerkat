// Package s3api builds the one S3 client shape meerkat uses — for a
// type: s3 content source and for an S3-backed memory store — and owns
// the two error translations both callers need.
//
// It is deliberately tiny. Everything provider-specific that meerkat
// relies on (path-style addressing for Garage, virtual-host addressing
// for AWS, conditional writes, opaque ETags) is decided here once, so
// the two packages that speak S3 agree on it, and docs/design/
// object-stores.md has a single place to point at.
//
// Credentials are the AWS default chain and nothing else: environment
// variables, the shared config/credentials files, IRSA / web identity,
// an EC2 or ECS metadata endpoint. There is no field anywhere in
// meerkat's schema for a static access key, mirroring the ADC-only rule
// the GCS backend has had from the start.
package s3api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Config is what a content source or memory spec contributes to the
// client: where the store is and how to address a bucket in it.
type Config struct {
	// Endpoint overrides the AWS endpoint, e.g. "https://s3.example.net"
	// for Garage or Versity Gateway. Empty means AWS.
	Endpoint string
	// Region is the signing region. Empty defers to the default chain
	// (AWS_REGION, the shared config); a region must come from
	// somewhere, and New fails clearly if none does.
	Region string
	// PathStyle addresses a bucket as <endpoint>/<bucket>/<key> instead
	// of <bucket>.<endpoint>/<key>. Garage needs it; AWS does not.
	PathStyle bool
}

// New builds an S3 client for cfg over the AWS default credential chain.
func New(ctx context.Context, cfg Config) (*s3.Client, error) {
	var opts []func(*config.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, config.WithRegion(cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load AWS configuration: %w — credentials resolve via the AWS default chain "+
			"(AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, ~/.aws/config, IRSA / web identity, instance metadata)", err)
	}
	if awsCfg.Region == "" {
		return nil, errors.New("s3: no region configured — set region: in the source (Garage: its s3_region, default \"garage\"), or AWS_REGION")
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
		// Only compute and validate request/response checksums where the
		// operation requires them. The SDK's default ("when supported")
		// sends CRC32 trailers with aws-chunked encoding on every upload,
		// which several third-party S3 servers reject or ignore; meerkat
		// pins content by ETag and by sha256 of its own, so the extra
		// integrity layer buys nothing here and costs compatibility.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return client, nil
}

// IsNotFound reports whether err is the backend saying the object (or
// the bucket) does not exist.
func IsNotFound(err error) bool {
	if hasCode(err, "NoSuchKey", "NotFound", "NoSuchBucket") {
		return true
	}
	return httpStatus(err) == http.StatusNotFound
}

// IsPreconditionFailed reports whether err is the backend refusing a
// conditional request: an If-Match that no longer matched, or an
// If-None-Match: * against an object that now exists. AWS may also
// answer 409 ConditionalRequestConflict when two conditional writes
// race; for an optimistic-locking caller that is the same news.
func IsPreconditionFailed(err error) bool {
	if hasCode(err, "PreconditionFailed", "ConditionalRequestConflict") {
		return true
	}
	switch httpStatus(err) {
	case http.StatusPreconditionFailed, http.StatusConflict:
		return true
	}
	return false
}

func hasCode(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.ErrorCode()
	for _, c := range codes {
		if code == c {
			return true
		}
	}
	return false
}

func httpStatus(err error) int {
	var rerr *awshttp.ResponseError
	if errors.As(err, &rerr) {
		return rerr.HTTPStatusCode()
	}
	return 0
}

// CleanETag returns an ETag header value without its quotes, or "" for
// a nil pointer. The value is an opaque version token: for a single-part
// upload most providers make it the MD5 of the body, for a multipart
// upload it is a hash of part hashes with a "-N" suffix, and under
// SSE-KMS it is neither. meerkat never interprets it — it only compares
// it and sends it back in If-Match.
func CleanETag(v *string) string {
	if v == nil {
		return ""
	}
	return strings.Trim(*v, `"`)
}

// QuoteETag is the inverse of CleanETag, for an If-Match header. S3
// compares the quoted form; sending it bare is accepted by AWS but not
// by every implementation.
func QuoteETag(etag string) string {
	if etag == "" {
		return ""
	}
	return `"` + strings.Trim(etag, `"`) + `"`
}
