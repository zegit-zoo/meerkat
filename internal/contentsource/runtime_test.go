package contentsource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- ResolveFlag ---

func TestResolveFlag_FlagWinsOverEnv(t *testing.T) {
	t.Setenv(EnvVar, "/env/path")
	if got := ResolveFlag("/flag/path"); got != "/flag/path" {
		t.Errorf("ResolveFlag = %q, want /flag/path", got)
	}
}

func TestResolveFlag_FallsBackToEnv(t *testing.T) {
	t.Setenv(EnvVar, "/env/path")
	if got := ResolveFlag(""); got != "/env/path" {
		t.Errorf("ResolveFlag = %q, want /env/path", got)
	}
}

func TestResolveFlag_EmptyWhenNeitherSet(t *testing.T) {
	t.Setenv(EnvVar, "")
	if got := ResolveFlag(""); got != "" {
		t.Errorf("ResolveFlag = %q, want empty", got)
	}
}

// --- LocateRuntime ---

func TestLocateRuntime_ExplicitFound(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "content-source.yaml")
	write(t, cfg, "content:\n  type: none\n")

	got, err := LocateRuntime(cfg)
	if err != nil {
		t.Fatalf("LocateRuntime: %v", err)
	}
	if got != cfg {
		t.Errorf("path = %q, want %q", got, cfg)
	}
}

func TestLocateRuntime_ExplicitMissingErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := LocateRuntime(missing)
	if err == nil {
		t.Fatal("expected an error for a nonexistent explicit path")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the path %q, got: %v", missing, err)
	}
}

func TestLocateRuntime_NoneFound(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	isolateConfigDir(t, t.TempDir())

	got, err := LocateRuntime("")
	if err != nil {
		t.Fatalf("LocateRuntime: %v", err)
	}
	if got != "" {
		t.Errorf("path = %q, want empty (nothing found)", got)
	}
}

func TestLocateRuntime_CWD(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), "content:\n  type: none\n")
	t.Chdir(dir)
	isolateConfigDir(t, t.TempDir()) // must not shadow the cwd file

	got, err := LocateRuntime("")
	if err != nil {
		t.Fatalf("LocateRuntime: %v", err)
	}
	if got != ConfigFile {
		t.Errorf("path = %q, want %q (relative, cwd-resolved)", got, ConfigFile)
	}
}

// TestLocateRuntime_UserConfigDir confirms the user-config-dir candidate
// is discovered, and that it takes priority over ./content-source.yaml
// (step 3 before step 4). Uses os.UserConfigDir() itself (after pointing
// HOME/XDG_CONFIG_HOME at a fresh temp dir) to compute the expected path
// portably, rather than hardcoding one platform's layout.
func TestLocateRuntime_UserConfigDir(t *testing.T) {
	home := t.TempDir()
	isolateConfigDir(t, home)
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("os.UserConfigDir unavailable: %v", err)
	}
	want := filepath.Join(base, "meerkat", ConfigFile)
	write(t, want, "content:\n  type: none\n")

	// Also plant a ./content-source.yaml in the cwd -- the user-config-dir
	// entry must win (it's checked first).
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, ConfigFile), "content:\n  type: local\n  path: should-not-be-used\n")
	t.Chdir(cwd)

	got, err := LocateRuntime("")
	if err != nil {
		t.Fatalf("LocateRuntime: %v", err)
	}
	if got != want {
		t.Errorf("path = %q, want %q (user config dir over cwd)", got, want)
	}
}

// isolateConfigDir points os.UserConfigDir() at base for the duration of
// the test (via HOME on Darwin/plan9, XDG_CONFIG_HOME/HOME on other
// Unix, AppData on Windows) so tests never touch the real developer
// machine's config directory and always start from a clean slate.
func isolateConfigDir(t *testing.T, base string) {
	t.Helper()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	t.Setenv("AppData", filepath.Join(base, "AppData", "Roaming"))
}

// --- LoadFile ---

func TestLoadFile_MissingErrors(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoadFile_InvalidYAMLErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), ConfigFile)
	write(t, p, "content: [this is not a valid source mapping\n")
	_, err := LoadFile(p)
	if err == nil {
		t.Fatal("expected a parse error for invalid YAML")
	}
}

func TestLoadFile_AppliesLayoutDefaultsAndValidates(t *testing.T) {
	p := filepath.Join(t.TempDir(), ConfigFile)
	write(t, p, "content:\n  type: local\n  path: ./kb\n")
	cfg, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	l := cfg.Content.Layout
	if l.Wiki != "wiki" || l.Sources != "ingestion/sources.yaml" || l.Prompts != "ingestion/prompts" || l.Templates != "templates" {
		t.Errorf("layout defaults not applied: %+v", l)
	}
}

func TestLoadFile_InvalidSourceErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), ConfigFile)
	write(t, p, "content:\n  type: url\n  url: https://example.com/kb.tar.gz\n") // missing sha256
	if _, err := LoadFile(p); err == nil {
		t.Fatal("expected a validation error for a url source with no sha256")
	}
}

// --- ResolveRuntime ---

func TestResolveRuntime_NothingFoundIsEmbeddedFallback(t *testing.T) {
	t.Chdir(t.TempDir())
	isolateConfigDir(t, t.TempDir())

	rc, err := ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if rc.Dir != "" {
		t.Errorf("Dir = %q, want empty (embedded fallback)", rc.Dir)
	}
}

func TestResolveRuntime_TypeNoneIsEmbeddedFallback(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), "content:\n  type: none\n")
	t.Chdir(dir)
	isolateConfigDir(t, t.TempDir())

	rc, err := ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if rc.Dir != "" {
		t.Errorf("Dir = %q, want empty (type: none -> embedded fallback)", rc.Dir)
	}
}

func TestResolveRuntime_TypeLocal_RelativeResolvedAgainstConfigFileDir(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "kb", "wiki", "index.md"), "home\n")
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: local\n  path: kb\n")

	rc, err := ResolveRuntime(cfgPath)
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	want := filepath.Join(root, "kb")
	if rc.Dir != want {
		t.Errorf("Dir = %q, want %q (resolved against the config file's own directory)", rc.Dir, want)
	}
	if rc.Source.Type != TypeLocal {
		t.Errorf("Source.Type = %q, want local", rc.Source.Type)
	}
}

func TestResolveRuntime_TypeLocal_AbsolutePathUsedAsIs(t *testing.T) {
	absDir := t.TempDir()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: local\n  path: "+absDir+"\n")

	rc, err := ResolveRuntime(cfgPath)
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if rc.Dir != absDir {
		t.Errorf("Dir = %q, want %q", rc.Dir, absDir)
	}
}

func TestResolveRuntime_TypeGitRejectedAsBuildTimeOnly(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: git\n  repo: o/r\n  ref: v1.0.0\n")

	_, err := ResolveRuntime(cfgPath)
	if err == nil {
		t.Fatal("expected an error for type: git at runtime")
	}
	if !strings.Contains(err.Error(), "git") || !strings.Contains(err.Error(), "build-time only") {
		t.Errorf("error = %v, want it to name the type and say build-time only", err)
	}
}

func TestResolveRuntime_TypeSubmoduleRejectedAsBuildTimeOnly(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: submodule\n  submodule: kb\n")

	_, err := ResolveRuntime(cfgPath)
	if err == nil {
		t.Fatal("expected an error for type: submodule at runtime")
	}
	if !strings.Contains(err.Error(), "submodule") || !strings.Contains(err.Error(), "build-time only") {
		t.Errorf("error = %v, want it to name the type and say build-time only", err)
	}
}

func TestResolveRuntime_ExplicitMissingPathErrors(t *testing.T) {
	_, err := ResolveRuntime(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected an error for a nonexistent --content-source path")
	}
}

// TestResolveRuntime_TypeURL_EndToEnd drives the full runtime path for a
// type: url source: a content-source.yaml naming a real HTTPS URL,
// resolved via ResolveRuntime (which internally calls FetchURL), landing
// on a directory containing the real extracted archive.
func TestResolveRuntime_TypeURL_EndToEnd(t *testing.T) {
	isolateCaches(t)
	body, digest := contentRepoTarGz(t)
	srv, hits := serveOnce(t, body)
	useTestServerClient(t, srv)

	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	write(t, cfgPath, "content:\n  type: url\n  url: "+srv.URL+"/kb.tar.gz\n  sha256: "+digest+"\n")

	rc, err := ResolveRuntime(cfgPath)
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if rc.Source.Type != TypeURL {
		t.Errorf("Source.Type = %q, want url", rc.Source.Type)
	}
	if *hits != 1 {
		t.Fatalf("hits = %d, want 1", *hits)
	}
	got, err := os.ReadFile(filepath.Join(rc.Dir, "wiki", "index.md"))
	if err != nil || !strings.Contains(string(got), "from the archive") {
		t.Fatalf("extracted content missing/wrong: %q err=%v", got, err)
	}

	prov := URLProvenance(rc.Source)
	if !strings.HasPrefix(prov, "url:"+srv.URL) {
		t.Errorf("URLProvenance = %q, want it to start with the source URL", prov)
	}
}

func TestResolveRuntime_TypeURL_ShaMismatchSurfacesThroughResolveRuntime(t *testing.T) {
	isolateCaches(t)
	body, _ := contentRepoTarGz(t)
	srv, _ := serveOnce(t, body)
	useTestServerClient(t, srv)

	root := t.TempDir()
	cfgPath := filepath.Join(root, "content-source.yaml")
	wrongDigest := strings.Repeat("0", 64)
	write(t, cfgPath, "content:\n  type: url\n  url: "+srv.URL+"/kb.tar.gz\n  sha256: "+wrongDigest+"\n")

	_, err := ResolveRuntime(cfgPath)
	if err == nil {
		t.Fatal("expected an error for a sha256 mismatch surfaced through ResolveRuntime")
	}
	if !strings.Contains(err.Error(), cfgPath) {
		t.Errorf("error = %v, want it to name the content-source.yaml path", err)
	}
}
