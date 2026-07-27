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

// TestRedirectAllowlist_RequiresHTTPS is the regression test for the
// missing-scheme-check finding: Go's http.Client only strips the
// Authorization header on a redirect to a different HOST, not on a
// same-host scheme downgrade, so checking the hostname alone let an
// https->http redirect on an otherwise-allowed host carry the bearer
// token to a plaintext connection. Every allowlisted host must now
// also be rejected over plain http.
func TestRedirectAllowlist_RequiresHTTPS(t *testing.T) {
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "api.github.com"}}}

	httpHosts := []string{
		"github.com",
		"api.github.com",
		"objects.githubusercontent.com",
		"releases.githubusercontent.com",
	}
	for _, host := range httpHosts {
		req := &http.Request{URL: &url.URL{Scheme: "http", Host: host}}
		if err := updateHTTPClient.CheckRedirect(req, via); err == nil {
			t.Errorf("expected plain-http redirect to allowlisted host %q to be rejected", host)
		}
	}

	// Sanity check: the same hosts over https still pass (otherwise
	// this test would trivially pass by rejecting everything).
	for _, host := range httpHosts {
		req := &http.Request{URL: &url.URL{Scheme: "https", Host: host}}
		if err := updateHTTPClient.CheckRedirect(req, via); err != nil {
			t.Errorf("expected https redirect to allowlisted host %q to pass, got %v", host, err)
		}
	}

	// Scheme comparison must be case-insensitive (URL schemes are
	// case-insensitive per RFC 3986).
	req := &http.Request{URL: &url.URL{Scheme: "HTTPS", Host: "github.com"}}
	if err := updateHTTPClient.CheckRedirect(req, via); err != nil {
		t.Errorf("expected uppercase HTTPS scheme to pass, got %v", err)
	}
}
