package traversal

import (
	"bytes"
	"context"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zegit-zoo/meerkat/internal/s3api"
)

// s3Sink writes each session as its own object. Keys are unique, so
// no write is ever conditional and every provider in
// docs/design/object-stores.md is safe.
type s3Sink struct {
	c      *s3.Client
	bucket string
	// prefix is the operator's prefix plus Prefix, ending in "/".
	prefix string
}

// NewS3Sink builds the S3 sink Open uses for backend: s3. Separate
// from Open so the package's core has no SDK dependency in tests.
func NewS3Sink(ctx context.Context, cfg *Config) (Sink, error) {
	c, err := s3api.New(ctx, s3api.Config{Endpoint: cfg.Endpoint, Region: cfg.Region, PathStyle: cfg.PathStyle})
	if err != nil {
		return nil, err
	}
	p := strings.TrimPrefix(cfg.Prefix, "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return &s3Sink{c: c, bucket: cfg.Bucket, prefix: p + Prefix}, nil
}

func (s *s3Sink) key(rel string) string { return s.prefix + strings.TrimPrefix(rel, Prefix) }

func (s *s3Sink) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.key(key)),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("application/json"),
	})
	return err
}

func (s *s3Sink) Days(ctx context.Context) ([]string, error) {
	out, err := s.c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix), Delimiter: aws.String("/"),
	})
	if err != nil {
		return nil, err
	}
	var days []string
	for _, p := range out.CommonPrefixes {
		d := path.Base(strings.TrimSuffix(aws.ToString(p.Prefix), "/"))
		if isDay(d) {
			days = append(days, d)
		}
	}
	return days, nil
}

func (s *s3Sink) DeleteDay(ctx context.Context, day string) error {
	if !isDay(day) {
		return nil
	}
	pager := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix + day + "/")})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			if _, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: o.Key}); err != nil {
				return err
			}
		}
	}
	return nil
}
