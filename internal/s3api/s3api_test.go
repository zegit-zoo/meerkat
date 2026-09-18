package s3api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func TestCleanAndQuoteETag(t *testing.T) {
	quoted := `"d41d8cd98f00b204e9800998ecf8427e"`
	if got := CleanETag(&quoted); got != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("CleanETag = %q", got)
	}
	if got := CleanETag(nil); got != "" {
		t.Errorf("CleanETag(nil) = %q, want empty", got)
	}
	if got := QuoteETag("abc-2"); got != `"abc-2"` {
		t.Errorf("QuoteETag = %q", got)
	}
	if got := QuoteETag(`"abc"`); got != `"abc"` {
		t.Errorf("QuoteETag must not double-quote, got %q", got)
	}
	if got := QuoteETag(""); got != "" {
		t.Errorf("QuoteETag(\"\") = %q, want empty", got)
	}
}

func TestErrorTranslation(t *testing.T) {
	byCode := func(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }
	byStatus := func(status int) error {
		return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
			Err:      errors.New("http"),
		}}
	}
	cases := []struct {
		name              string
		err               error
		notFound, precond bool
	}{
		{"NoSuchKey", byCode("NoSuchKey"), true, false},
		{"NotFound", byCode("NotFound"), true, false},
		{"NoSuchBucket", byCode("NoSuchBucket"), true, false},
		{"404", byStatus(http.StatusNotFound), true, false},
		{"PreconditionFailed", byCode("PreconditionFailed"), false, true},
		{"ConditionalRequestConflict", byCode("ConditionalRequestConflict"), false, true},
		{"412", byStatus(http.StatusPreconditionFailed), false, true},
		{"409", byStatus(http.StatusConflict), false, true},
		{"AccessDenied", byCode("AccessDenied"), false, false},
		{"500", byStatus(http.StatusInternalServerError), false, false},
		{"plain", errors.New("dial tcp: connection refused"), false, false},
		{"nil", nil, false, false},
	}
	for _, c := range cases {
		if got := IsNotFound(c.err); got != c.notFound {
			t.Errorf("%s: IsNotFound = %v, want %v", c.name, got, c.notFound)
		}
		if got := IsPreconditionFailed(c.err); got != c.precond {
			t.Errorf("%s: IsPreconditionFailed = %v, want %v", c.name, got, c.precond)
		}
	}
}

func TestNew_NeedsARegionFromSomewhere(t *testing.T) {
	// Isolate from the developer's real AWS environment and config.
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	if _, err := New(context.Background(), Config{Endpoint: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("New with no region anywhere must fail rather than sign requests for an unknown region")
	}
	c, err := New(context.Background(), Config{Endpoint: "http://127.0.0.1:1", Region: "garage", PathStyle: true})
	if err != nil {
		t.Fatalf("New with an explicit region: %v", err)
	}
	opts := c.Options()
	if opts.BaseEndpoint == nil || *opts.BaseEndpoint != "http://127.0.0.1:1" {
		t.Errorf("BaseEndpoint not applied: %v", opts.BaseEndpoint)
	}
	if !opts.UsePathStyle {
		t.Error("UsePathStyle not applied")
	}
	if opts.Region != "garage" {
		t.Errorf("Region = %q", opts.Region)
	}
}
