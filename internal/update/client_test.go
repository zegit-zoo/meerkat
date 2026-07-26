package update

import (
	"net/http"
	"net/url"
	"testing"
)

func TestRedirectAllowlist(t *testing.T) {
	// api.github.com redirects to *.githubusercontent.com CDN for asset downloads.
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "objects.githubusercontent.com"}}
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "api.github.com"}}}
	if err := updateHTTPClient.CheckRedirect(req, via); err != nil {
		t.Fatalf("expected *.githubusercontent.com redirect to pass, got %v", err)
	}

	req = &http.Request{URL: &url.URL{Scheme: "https", Host: "api.github.com"}}
	if err := updateHTTPClient.CheckRedirect(req, via); err != nil {
		t.Fatalf("expected api.github.com redirect to pass, got %v", err)
	}

	req = &http.Request{URL: &url.URL{Scheme: "https", Host: "github.com"}}
	if err := updateHTTPClient.CheckRedirect(req, via); err != nil {
		t.Fatalf("expected github.com redirect to pass, got %v", err)
	}

	req = &http.Request{URL: &url.URL{Scheme: "https", Host: "releases.githubusercontent.com"}}
	if err := updateHTTPClient.CheckRedirect(req, via); err != nil {
		t.Fatalf("expected releases.githubusercontent.com redirect to pass, got %v", err)
	}

	req = &http.Request{URL: &url.URL{Scheme: "https", Host: "evil.example"}}
	if err := updateHTTPClient.CheckRedirect(req, via); err == nil {
		t.Fatal("expected non-github redirect to be rejected")
	}
}
