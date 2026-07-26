package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/auth"
)

// withStubbedGitHubAPI points githubAPIBase at srv for the duration of the
// test and restores it afterward.
func withStubbedGitHubAPI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = orig })
}

// withStubbedToken overrides resolveGitHubToken for the duration of the
// test, so fetchOne's token resolution doesn't depend on this machine's
// real gh CLI login state.
func withStubbedToken(t *testing.T, tok string, err error) {
	t.Helper()
	orig := resolveGitHubToken
	resolveGitHubToken = func() (string, error) { return tok, err }
	t.Cleanup(func() { resolveGitHubToken = orig })
}

// TestFetchLatest_AnonymousWorksWithoutToken: the repo is public now, so
// FetchLatest must succeed with no Authorization header sent at all when
// no token is cached — this is the fix for the former hard requirement.
func TestFetchLatest_AnonymousWorksWithoutToken(t *testing.T) {
	withStubbedToken(t, "", auth.ErrNoConfig)

	var sawAuthHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuthHeader = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.5.0","name":"v0.5.0","published_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	withStubbedGitHubAPI(t, srv)

	rel, err := FetchLatest(context.Background())
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if rel.TagName != "v0.5.0" {
		t.Errorf("TagName = %q", rel.TagName)
	}
	if sawAuthHeader {
		t.Error("expected no Authorization header when no token is cached")
	}
}

// TestFetchLatest_SendsTokenWhenCached: when a token IS cached we still
// send it (for the higher authenticated rate limit), even though the
// repo no longer requires it.
func TestFetchLatest_SendsTokenWhenCached(t *testing.T) {
	withStubbedToken(t, "ghs_cachedtoken", nil)

	var gotAuthHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.5.0"}`))
	}))
	defer srv.Close()
	withStubbedGitHubAPI(t, srv)

	if _, err := FetchLatest(context.Background()); err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if gotAuthHeader != "Bearer ghs_cachedtoken" {
		t.Errorf("Authorization = %q, want Bearer ghs_cachedtoken", gotAuthHeader)
	}
}

// TestFetchLatest_403Anonymous_MentionsRateLimit: an anonymous 403/401
// should point at the rate limit rather than implying auth is mandatory.
func TestFetchLatest_403Anonymous_MentionsRateLimit(t *testing.T) {
	withStubbedToken(t, "", auth.ErrNoConfig)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()
	withStubbedGitHubAPI(t, srv)

	_, err := FetchLatest(context.Background())
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("expected error to mention the rate limit, got: %v", err)
	}
}

// TestPickAssetName: matches goreleaser's actual output (no `v` in
// the version segment of the filename, despite the tag having one).
func TestPickAssetName(t *testing.T) {
	cases := map[string]string{
		"v0.4.0": "meerkat_0.4.0_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz",
		"0.4.0":  "meerkat_0.4.0_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz",
	}
	for in, want := range cases {
		if got := PickAssetName(in); got != want {
			t.Errorf("PickAssetName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestChecksumAssetName.
func TestChecksumAssetName(t *testing.T) {
	if got, want := ChecksumAssetName("v0.4.0"), "meerkat_0.4.0_checksums.txt"; got != want {
		t.Errorf("ChecksumAssetName = %q, want %q", got, want)
	}
}

// TestRelease_FindAsset: returns the asset's API URL when found by name.
func TestRelease_FindAsset(t *testing.T) {
	apiURL := "https://api.github.com/repos/zegit-zoo/meerkat/releases/assets/12345"
	rel := &Release{
		TagName: "v0.4.0",
		Assets: []Asset{
			{Name: "meerkat_0.4.0_linux_amd64.tar.gz", URL: apiURL, Size: 1234},
			{Name: "meerkat_0.4.0_checksums.txt", URL: "https://api.github.com/repos/zegit-zoo/meerkat/releases/assets/12346", Size: 512},
		},
	}

	got, ok := rel.FindAsset("meerkat_0.4.0_linux_amd64.tar.gz")
	if !ok {
		t.Fatal("FindAsset returned !ok for present asset")
	}
	if got != apiURL {
		t.Errorf("FindAsset URL = %q, want %q", got, apiURL)
	}

	// Missing asset should return !ok.
	_, ok = rel.FindAsset("meerkat_0.4.0_windows_amd64.zip")
	if ok {
		t.Error("FindAsset should return !ok for missing asset")
	}

	// Empty assets slice should return !ok.
	empty := &Release{TagName: "v0.4.0"}
	if _, ok := empty.FindAsset("anything"); ok {
		t.Error("FindAsset on empty Assets should return !ok")
	}
}

// TestRelease_FindAsset_ChecksumsAndCosign: all three cosign-related assets
// are found by exact name.
func TestRelease_FindAsset_ChecksumsAndCosign(t *testing.T) {
	sigURL := "https://api.github.com/repos/zegit-zoo/meerkat/releases/assets/20001"
	certURL := "https://api.github.com/repos/zegit-zoo/meerkat/releases/assets/20002"
	rel := &Release{
		TagName: "v0.4.2",
		Assets: []Asset{
			{Name: "meerkat_0.4.2_checksums.txt.sig", URL: sigURL},
			{Name: "meerkat_0.4.2_checksums.txt.pem", URL: certURL},
		},
	}
	if u, ok := rel.FindAsset("meerkat_0.4.2_checksums.txt.sig"); !ok || u != sigURL {
		t.Errorf("sig: ok=%v url=%q", ok, u)
	}
	if u, ok := rel.FindAsset("meerkat_0.4.2_checksums.txt.pem"); !ok || u != certURL {
		t.Errorf("cert: ok=%v url=%q", ok, u)
	}
}
