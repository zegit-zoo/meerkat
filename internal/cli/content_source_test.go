package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
)

// These tests exercise --content-source / MEERKAT_CONTENT_SOURCE end to
// end through the real cobra command tree, mirroring
// kbdir_flag_test.go's approach for --kb-dir. internal/contentsource's
// own package tests already cover ResolveRuntime/LoadFile/FetchURL in
// isolation; what's under test here is root.go's wiring: flag/env
// precedence against --kb-dir, and that a resolved directory/error
// actually reaches kb/sources/mk version the same way --kb-dir does.

// writeKBTarGz builds a real content-repo-layout tar.gz (wiki pages +
// an ingestion source registry) and returns its bytes plus hex sha256 —
// the "content archive an operator's HTTPS host would serve" for a
// type: url content-source.yaml.
func writeKBTarGz(t *testing.T) (body []byte, sha256hex string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"wiki/index.md":            "---\nid: index\ntitle: Index\n---\n# Index\n\nserved from the url archive.\n",
		"wiki/concepts/widgets.md": "---\nid: concepts/widgets\ntitle: Widgets\n---\n# Widgets\n\nurl-archive widgets body.\n",
		"ingestion/sources.yaml":   "sources: []\n",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("WriteHeader(%q): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	body = buf.Bytes()
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])
}

// trustTestServer swaps the process-wide http.DefaultTransport (which
// contentsource's urlHTTPClient — unexported, in a different package —
// falls back to since it never sets its own Transport) to one that
// trusts srv's self-signed certificate, for the duration of the test.
// This drives the REAL fetch->verify->extract->serve cycle (real HTTPS,
// real net/http client, real production redirect/scheme policy) through
// the actual `mk`/cobra command tree without needing any test-only seam
// in internal/contentsource's public API.
func trustTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// isolateContentCaches points os.UserCacheDir()/os.UserConfigDir() at a
// fresh temp dir so a type: url fetch's cache, and content-source.yaml
// discovery (step 3 of the resolution order), never touch the real
// developer machine's directories during a test.
func isolateContentCaches(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func TestContentSourceFlag_TypeLocal_ServesContentAndReportsDiskProvenance(t *testing.T) {
	resetKBToEmbedded(t)
	fixture := newFixtureKBDir(t)
	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: local\n  path: "+fixture+"\n")

	listOut, err := execRoot(t, "--content-source", cfgPath, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, listOut)
	}
	if !strings.Contains(listOut, "concepts/widgets") {
		t.Errorf("list output missing fixture page: %s", listOut)
	}

	verOut, err := execRoot(t, "--content-source", cfgPath, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, verOut)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(verOut), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, verOut)
	}
	if want := "disk:" + fixture; info.KBSource != want {
		t.Errorf("kb_source = %q, want %q", info.KBSource, want)
	}
}

func TestContentSourceFlag_KBDirWinsOverContentSource(t *testing.T) {
	resetKBToEmbedded(t)
	kbDir := newFixtureKBDir(t)

	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	// Points at a path that doesn't exist -- if --content-source were
	// ever consulted, this would surface as an error. --kb-dir must win
	// outright, so content-source.yaml is never even read.
	write(t, cfgPath, "content:\n  type: local\n  path: /does/not/exist/at/all\n")

	out, err := execRoot(t, "--kb-dir", kbDir, "--content-source", cfgPath, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if want := "disk:" + kbDir; info.KBSource != want {
		t.Errorf("kb_source = %q, want %q (--kb-dir should win over --content-source)", info.KBSource, want)
	}
}

func TestContentSourceFlag_EnvVar(t *testing.T) {
	resetKBToEmbedded(t)
	fixture := newFixtureKBDir(t)
	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: local\n  path: "+fixture+"\n")
	t.Setenv(contentsource.EnvVar, cfgPath)

	out, err := execRoot(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if want := "disk:" + fixture; info.KBSource != want {
		t.Errorf("kb_source = %q, want %q", info.KBSource, want)
	}
}

func TestContentSourceFlag_TypeGitRejected_BuildTimeOnlyError(t *testing.T) {
	resetKBToEmbedded(t)
	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: git\n  repo: o/r\n  ref: v1.0.0\n")

	_, err := execRoot(t, "--content-source", cfgPath, "version")
	if err == nil {
		t.Fatal("expected an error for type: git at runtime")
	}
	if !strings.Contains(err.Error(), "build-time only") {
		t.Errorf("error = %v, want it to say build-time only", err)
	}
}

func TestContentSourceFlag_NonexistentPathErrors(t *testing.T) {
	resetKBToEmbedded(t)
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	_, err := execRoot(t, "--content-source", missing, "version")
	if err == nil {
		t.Fatal("expected an error for a nonexistent --content-source path")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the path %q, got: %v", missing, err)
	}
}

// TestContentSourceFlag_DiscoveredFromWorkingDirectory: with neither
// --kb-dir nor --content-source given, a ./content-source.yaml in the
// working directory (resolution step 4) must still be picked up.
func TestContentSourceFlag_DiscoveredFromWorkingDirectory(t *testing.T) {
	resetKBToEmbedded(t)
	isolateContentCaches(t) // keep step 3 (user config dir) from shadowing this test
	fixture := newFixtureKBDir(t)
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, "content-source.yaml"), "content:\n  type: local\n  path: "+fixture+"\n")
	t.Chdir(cwd)

	out, err := execRoot(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	if !strings.Contains(out, "concepts/widgets") {
		t.Errorf("list output missing fixture page (cwd content-source.yaml not discovered): %s", out)
	}
}

// TestContentSourceFlag_TypeURL_EndToEnd_RealFetchVerifyExtractServe is
// the full real cycle: an HTTPS server (real TLS handshake, real
// net/http client, real production redirect/scheme policy) serves a
// content archive; `mk --content-source <cfg> list/search/show` (the
// real cobra command tree) fetches, verifies its sha256, extracts it,
// and serves pages that came from the archive; `mk version --json`
// reports the url: provenance.
func TestContentSourceFlag_TypeURL_EndToEnd_RealFetchVerifyExtractServe(t *testing.T) {
	resetKBToEmbedded(t)
	isolateContentCaches(t)
	body, digest := writeKBTarGz(t)

	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	trustTestServer(t, srv)

	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	cfgBody := "content:\n  type: url\n  url: " + srv.URL + "/kb-v1.tar.gz\n  sha256: " + digest + "\n"
	write(t, cfgPath, cfgBody)
	t.Logf("content-source.yaml:\n%s", cfgBody)

	listOut, err := execRoot(t, "--content-source", cfgPath, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, listOut)
	}
	t.Logf("$ mk --content-source content-source.yaml list --json\n%s", listOut)
	if !strings.Contains(listOut, "concepts/widgets") {
		t.Errorf("list output missing archive page: %s", listOut)
	}

	searchOut, err := execRoot(t, "--content-source", cfgPath, "search", "widgets")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, searchOut)
	}
	t.Logf("$ mk --content-source content-source.yaml search widgets\n%s", searchOut)
	if !strings.Contains(searchOut, "concepts/widgets") {
		t.Errorf("search output missing archive page: %s", searchOut)
	}

	showOut, err := execRoot(t, "--content-source", cfgPath, "show", "concepts/widgets")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, showOut)
	}
	t.Logf("$ mk --content-source content-source.yaml show concepts/widgets\n%s", showOut)
	if !strings.Contains(showOut, "url-archive widgets body") {
		t.Errorf("show output missing archive page body: %s", showOut)
	}

	verOut, err := execRoot(t, "--content-source", cfgPath, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, verOut)
	}
	t.Logf("$ mk --content-source content-source.yaml version --json\n%s", verOut)
	var info versionInfo
	if err := json.Unmarshal([]byte(verOut), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, verOut)
	}
	wantPrefix := "url:" + srv.URL + "/kb-v1.tar.gz@" + digest[:12]
	if info.KBSource != wantPrefix {
		t.Errorf("kb_source = %q, want %q", info.KBSource, wantPrefix)
	}
	// kb_commit stays the build-time embedded value -- a url: source is
	// content-verified but has no build-time pin of its own.
	if info.KBCommit != kbCommit {
		t.Errorf("kb_commit changed to %q under type: url; must stay the build-time embed value %q", info.KBCommit, kbCommit)
	}

	// Every command above hit the server exactly once each on first use,
	// then subsequent commands in this same test process are separate
	// `mk` invocations (fresh cobra trees) but share the same on-disk
	// cache keyed by digest -- so only the very first invocation should
	// have downloaded anything.
	if hits != 1 {
		t.Errorf("server was hit %d times across 4 separate `mk` invocations sharing one cache, want 1 (cache hit path)", hits)
	}
}

func TestContentSourceFlag_TypeURL_ShaMismatch_ThroughCLI(t *testing.T) {
	resetKBToEmbedded(t)
	isolateContentCaches(t)
	body, _ := writeKBTarGz(t)
	wrongDigest := strings.Repeat("0", 64)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	trustTestServer(t, srv)

	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: url\n  url: "+srv.URL+"/kb.tar.gz\n  sha256: "+wrongDigest+"\n")

	_, err := execRoot(t, "--content-source", cfgPath, "version")
	t.Logf("$ mk --content-source content-source.yaml version\nerror: %v", err)
	if err == nil {
		t.Fatal("expected an error for a sha256 mismatch")
	}
	if !strings.Contains(err.Error(), wrongDigest) {
		t.Errorf("error = %v, want it to name the mismatched digest", err)
	}
}
