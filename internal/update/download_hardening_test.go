package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func assetPath(id string) string { return "/repos/zegit-zoo/meerkat/releases/assets/" + id }

func withAssetBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = orig })
}

// The token must never reach a URL outside the project's release-asset
// endpoint, even when GitHub-style rate limiting asks for it.
func TestDownloadAsset_TokenNotSentOutsideAssetPrefix(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, "rate limit", http.StatusForbidden)
	}))
	defer srv.Close()
	withAssetBase(t, srv)

	for _, p := range []string{"/other/path", "/repos/evil/meerkat/releases/assets/1", "/repos/zegit-zoo/meerkat/releases/latest"} {
		if _, _, err := DownloadAsset(context.Background(), srv.URL+p, "secret"); err == nil {
			t.Errorf("%s: expected error", p)
		}
	}
	if sawAuth {
		t.Fatal("token was sent to a non-asset URL")
	}
}

func TestTokenAllowedForAsset(t *testing.T) {
	orig := githubAPIBase
	githubAPIBase = "https://api.github.com"
	defer func() { githubAPIBase = orig }()
	ok := "https://api.github.com/repos/zegit-zoo/meerkat/releases/assets/123"
	if !tokenAllowedForAsset(ok) {
		t.Error("canonical asset URL must be allowed")
	}
	for _, bad := range []string{
		"http://api.github.com/repos/zegit-zoo/meerkat/releases/assets/123",
		"https://api.github.com.evil.example/repos/zegit-zoo/meerkat/releases/assets/123",
		"https://evil.example/https://api.github.com/repos/zegit-zoo/meerkat/releases/assets/123",
		"https://api.github.com/repos/zegit-zoo/other/releases/assets/123",
		"https://objects.githubusercontent.com/x",
	} {
		if tokenAllowedForAsset(bad) {
			t.Errorf("%q must not be allowed", bad)
		}
	}
}

func TestDownloadAsset_AnonymousFirstThenTokenOnRateLimit(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	withAssetBase(t, srv)

	p, _, err := DownloadAsset(context.Background(), srv.URL+assetPath("7"), "tok")
	if err != nil {
		t.Fatalf("DownloadAsset: %v", err)
	}
	os.Remove(p)
	if len(auths) != 2 || auths[0] != "" || auths[1] != "Bearer tok" {
		t.Errorf("auth sequence = %q", auths)
	}
}

func TestDownloadAsset_SizeCap(t *testing.T) {
	prev := maxDownloadBytes
	maxDownloadBytes = 32
	defer func() { maxDownloadBytes = prev }()

	t.Run("body over cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Chunked: no Content-Length to catch it early.
			fl := w.(http.Flusher)
			for i := 0; i < 4; i++ {
				_, _ = w.Write([]byte(strings.Repeat("x", 10)))
				fl.Flush()
			}
		}))
		defer srv.Close()
		_, _, err := DownloadAsset(context.Background(), srv.URL+"/x", "")
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err = %v, want size-limit error", err)
		}
	})
	t.Run("declared length over cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 100)))
		}))
		defer srv.Close()
		_, _, err := DownloadAsset(context.Background(), srv.URL+"/x", "")
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err = %v, want size-limit error", err)
		}
	})
	t.Run("at cap is accepted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 32)))
		}))
		defer srv.Close()
		p, _, err := DownloadAsset(context.Background(), srv.URL+"/x", "")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		os.Remove(p)
	})
}

// A stalled body is cut off by the idle timeout instead of hanging
// until the overall context expires.
func TestDownloadAsset_IdleTimeout(t *testing.T) {
	prev := bodyIdleTimeout
	bodyIdleTimeout = 100 * time.Millisecond
	defer func() { bodyIdleTimeout = prev }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	_, _, err := DownloadAsset(context.Background(), srv.URL+"/x", "")
	if err == nil {
		t.Fatal("expected error on stalled body")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v; idle timeout did not fire", time.Since(start))
	}
}

// A slow but steady body is not cut off, since there is no total-time
// limit on the body read.
func TestDownloadAsset_SlowSteadyBodySucceeds(t *testing.T) {
	prev := bodyIdleTimeout
	bodyIdleTimeout = 300 * time.Millisecond
	defer func() { bodyIdleTimeout = prev }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 6; i++ {
			_, _ = w.Write([]byte("ab"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()
	p, _, err := DownloadAsset(context.Background(), srv.URL+"/x", "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	b, _ := os.ReadFile(p)
	os.Remove(p)
	if len(b) != 12 {
		t.Errorf("got %d bytes", len(b))
	}
}

func TestUpdateClientHasNoTotalTimeoutButBoundedHeaders(t *testing.T) {
	if updateHTTPClient.Timeout != 0 {
		t.Errorf("client Timeout = %v; a whole-exchange timeout breaks slow downloads", updateHTTPClient.Timeout)
	}
	tr, ok := updateHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", updateHTTPClient.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 || tr.TLSHandshakeTimeout <= 0 || tr.DialContext == nil {
		t.Errorf("connect/header timeouts not set: %+v", tr)
	}
}
