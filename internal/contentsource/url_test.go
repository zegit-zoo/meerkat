package contentsource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// contentRepoTarGz builds a real content-repo-layout archive (the shape
// FetchURL's caller — ResolveRuntime, then internal/kbdir — expects to
// find inside the extracted directory) and returns its bytes plus hex
// sha256.
func contentRepoTarGz(t *testing.T) (body []byte, sha256hex string) {
	t.Helper()
	body = buildTarGz(t, []tarEntry{
		regEntry("wiki/index.md", "---\nid: index\ntitle: Index\n---\n# Index\n\nfrom the archive\n"),
		regEntry("wiki/concepts/widgets.md", "---\nid: concepts/widgets\ntitle: Widgets\n---\n# Widgets\n"),
		regEntry("ingestion/sources.yaml", "sources: []\n"),
		regEntry("templates/default.md", "---\n---\n"),
	})
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])
}

// serveOnce starts a TLS test server serving body for every request and
// returns the server plus a hit counter.
func serveOnce(t *testing.T, body []byte) (srv *httptest.Server, hits *int64) {
	t.Helper()
	hits = new(int64)
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// useTestServerClient points urlHTTPClient at srv's trusted transport
// for the duration of the test, restoring the real client on cleanup.
// The real CheckRedirect/Timeout are preserved — only the Transport
// (which is what decides whether the test server's self-signed cert is
// trusted) is swapped, so redirect-policy tests still exercise
// production logic.
func useTestServerClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := urlHTTPClient
	t.Cleanup(func() { urlHTTPClient = orig })
	urlHTTPClient = &http.Client{
		Transport:     srv.Client().Transport,
		Timeout:       orig.Timeout,
		CheckRedirect: orig.CheckRedirect,
	}
}

// isolateCaches points os.UserCacheDir() (via HOME/XDG_CACHE_HOME) at a
// fresh temp dir for the test, so FetchURL's cache never touches the
// real developer machine's cache and every test starts cache-empty.
func isolateCaches(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
}

func TestFetchURL_EndToEnd_HappyPath(t *testing.T) {
	isolateCaches(t)
	body, digest := contentRepoTarGz(t)
	srv, hits := serveOnce(t, body)
	useTestServerClient(t, srv)

	dir, err := FetchURL(Source{Type: TypeURL, URL: srv.URL + "/kb.tar.gz", SHA256: digest})
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if *hits != 1 {
		t.Fatalf("server hit %d times, want 1", *hits)
	}

	got, err := os.ReadFile(filepath.Join(dir, "wiki", "index.md"))
	if err != nil {
		t.Fatalf("read extracted wiki/index.md: %v", err)
	}
	if !strings.Contains(string(got), "from the archive") {
		t.Errorf("wiki/index.md = %q, missing expected content", got)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "ingestion", "sources.yaml")); err != nil {
		t.Errorf("read extracted ingestion/sources.yaml: %v", err)
	}
	if !isCacheComplete(dir) {
		t.Error("expected the cache dir to be marked complete after a successful fetch")
	}

	// Cache dir must be keyed by digest, under os.UserCacheDir().
	base, _ := os.UserCacheDir()
	wantDir := filepath.Join(base, "meerkat", "content", "url", digest)
	if dir != wantDir {
		t.Errorf("cache dir = %q, want %q", dir, wantDir)
	}
}

// TestFetchURL_CacheHit_NoSecondDownload proves the cache-hit path: a
// second FetchURL call with the same digest must not touch the network
// at all.
func TestFetchURL_CacheHit_NoSecondDownload(t *testing.T) {
	isolateCaches(t)
	body, digest := contentRepoTarGz(t)
	srv, hits := serveOnce(t, body)
	useTestServerClient(t, srv)

	src := Source{Type: TypeURL, URL: srv.URL + "/kb.tar.gz", SHA256: digest}
	dir1, err := FetchURL(src)
	if err != nil {
		t.Fatalf("first FetchURL: %v", err)
	}
	t.Logf("first FetchURL: dir=%s  server hits=%d", dir1, *hits)
	if *hits != 1 {
		t.Fatalf("after first fetch, hits = %d, want 1", *hits)
	}

	dir2, err := FetchURL(src)
	if err != nil {
		t.Fatalf("second FetchURL: %v", err)
	}
	t.Logf("second FetchURL (same digest): dir=%s  server hits=%d (unchanged -> no download happened)", dir2, *hits)
	if dir2 != dir1 {
		t.Errorf("second fetch returned a different dir: %q != %q", dir2, dir1)
	}
	if *hits != 1 {
		t.Fatalf("after second (cache-hit) fetch, hits = %d, want still 1 — a download happened when it should not have", *hits)
	}
}

// TestFetchURL_ShaMismatchRejected_NoCacheEntry: a digest that doesn't
// match the served bytes must be rejected, name both digests, and leave
// no cache directory at all — for either the wrong digest the config
// named, or (implicitly, since it's never created) the real one.
func TestFetchURL_ShaMismatchRejected_NoCacheEntry(t *testing.T) {
	isolateCaches(t)
	body, realDigest := contentRepoTarGz(t)
	srv, _ := serveOnce(t, body)
	useTestServerClient(t, srv)

	wrongDigest := strings.Repeat("0", 64)
	if wrongDigest == realDigest {
		t.Fatal("test setup bug: wrongDigest collides with realDigest")
	}
	t.Logf("content-source.yaml declares sha256=%s; server actually serves an archive whose real sha256=%s", wrongDigest, realDigest)

	_, err := FetchURL(Source{Type: TypeURL, URL: srv.URL + "/kb.tar.gz", SHA256: wrongDigest})
	if err == nil {
		t.Fatal("expected an error for a sha256 mismatch")
	}
	t.Logf("FetchURL error (nothing extracted or cached): %v", err)
	if !strings.Contains(err.Error(), wrongDigest) || !strings.Contains(err.Error(), realDigest) {
		t.Errorf("error = %v, want it to name both the wanted (%s) and got (%s) digests", err, wrongDigest, realDigest)
	}

	base, _ := os.UserCacheDir()
	for _, d := range []string{wrongDigest, realDigest} {
		p := filepath.Join(base, "meerkat", "content", "url", d)
		_, statErr := os.Stat(p)
		if !os.IsNotExist(statErr) {
			t.Errorf("cache dir %q exists after a sha256 mismatch — nothing should be cached", p)
		}
		t.Logf("cache dir %s: Stat = %v (does not exist)", p, statErr)
	}
}

func TestFetchURL_ServerErrorStatusRejected(t *testing.T) {
	isolateCaches(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	useTestServerClient(t, srv)

	_, err := FetchURL(Source{Type: TypeURL, URL: srv.URL + "/kb.tar.gz", SHA256: strings.Repeat("a", 64)})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

// TestFetchURL_RedirectToHTTPRejected proves the real (unmodified)
// CheckRedirect closure runs during FetchURL: a same-origin redirect to
// a plain http:// URL must be refused rather than followed.
func TestFetchURL_RedirectToHTTPRejected(t *testing.T) {
	isolateCaches(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/kb.tar.gz", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	useTestServerClient(t, srv)

	_, err := FetchURL(Source{Type: TypeURL, URL: srv.URL + "/kb.tar.gz", SHA256: strings.Repeat("a", 64)})
	if err == nil {
		t.Fatal("expected an error for a redirect to a non-https URL")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error = %v, want it to mention https", err)
	}
}

// TestFetchURL_TypeMismatchGuard: calling FetchURL directly (bypassing
// ResolveRuntime/LoadFile's own type switch) with a non-url Source must
// be rejected rather than doing anything.
func TestFetchURL_TypeMismatchGuard(t *testing.T) {
	_, err := FetchURL(Source{Type: TypeLocal, Path: "kb"})
	if err == nil {
		t.Fatal("expected an error for a non-url Source")
	}
}

// TestFetchURL_ValidationDefenseInDepth: FetchURL re-checks scheme and
// digest shape itself, in case it's ever called directly with a Source
// that skipped Source.Validate.
func TestFetchURL_ValidationDefenseInDepth(t *testing.T) {
	isolateCaches(t)
	cases := map[string]Source{
		"non-https url": {Type: TypeURL, URL: "http://example.com/kb.tar.gz", SHA256: strings.Repeat("a", 64)},
		"bad sha shape": {Type: TypeURL, URL: "https://example.com/kb.tar.gz", SHA256: "not-hex"},
		"empty sha":     {Type: TypeURL, URL: "https://example.com/kb.tar.gz"},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FetchURL(src); err == nil {
				t.Errorf("expected an error for %s", name)
			}
		})
	}
}

// TestFetchURL_DownloadCapEnforced: a response larger than
// maxDownloadBytes must be rejected before it's fully buffered to disk.
func TestFetchURL_DownloadCapEnforced(t *testing.T) {
	isolateCaches(t)
	origCap := maxDownloadBytes
	maxDownloadBytes = 1024
	t.Cleanup(func() { maxDownloadBytes = origCap })

	body := []byte(strings.Repeat("x", int(maxDownloadBytes)+4096))
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	srv, _ := serveOnce(t, body)
	useTestServerClient(t, srv)

	_, err := FetchURL(Source{Type: TypeURL, URL: srv.URL + "/big.tar.gz", SHA256: digest})
	if err == nil {
		t.Fatal("expected an error for a response exceeding the download cap")
	}
	if !strings.Contains(err.Error(), "download cap") {
		t.Errorf("error = %v, want it to mention the download cap", err)
	}
}

// TestFetchURL_IncompleteCacheDirIsNotTrusted: a cache directory that
// exists but lacks the completion marker (simulating a crash between
// MkdirTemp and the final rename in some earlier run, or any other means
// a partial directory might land at the cache path) must never be
// treated as a cache hit — FetchURL must redo the work rather than
// silently serving a possibly-incomplete tree.
func TestFetchURL_IncompleteCacheDirIsNotTrusted(t *testing.T) {
	isolateCaches(t)
	body, digest := contentRepoTarGz(t)
	srv, hits := serveOnce(t, body)
	useTestServerClient(t, srv)

	cacheDir, err := urlCacheDir(digest)
	if err != nil {
		t.Fatal(err)
	}
	// Plant a directory at the cache path with no completion marker and
	// no real content -- the "half-extracted" shape this whole mechanism
	// guards against.
	if err := os.MkdirAll(filepath.Join(cacheDir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := FetchURL(Source{Type: TypeURL, URL: srv.URL + "/kb.tar.gz", SHA256: digest})
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if *hits != 1 {
		t.Fatalf("expected FetchURL to redo the fetch for an incomplete cache dir, hits = %d", *hits)
	}
	got, err := os.ReadFile(filepath.Join(dir, "wiki", "index.md"))
	if err != nil || !strings.Contains(string(got), "from the archive") {
		t.Errorf("expected the real archive content after redoing an incomplete cache, got %q err=%v", got, err)
	}
}

func TestURLProvenance(t *testing.T) {
	got := URLProvenance(Source{URL: "https://example.com/kb.tar.gz", SHA256: strings.Repeat("ab", 32)})
	want := fmt.Sprintf("url:https://example.com/kb.tar.gz@%s", strings.Repeat("ab", 6))
	if got != want {
		t.Errorf("URLProvenance = %q, want %q", got, want)
	}
}

// TestURLProvenance_ShortDigestDoesNotPanic: defensive — Source.Validate
// never lets a short digest through in practice, but URLProvenance must
// not panic if it's ever handed one directly.
func TestURLProvenance_ShortDigestDoesNotPanic(t *testing.T) {
	got := URLProvenance(Source{URL: "https://example.com/kb.tar.gz", SHA256: "abcd"})
	if got != "url:https://example.com/kb.tar.gz@abcd" {
		t.Errorf("URLProvenance with a short digest = %q", got)
	}
}
