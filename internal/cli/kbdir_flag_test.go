package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kbdir"
)

// These tests exercise the --kb-dir / MEERKAT_KB_DIR wiring end-to-end
// through the real cobra command tree (NewRootCmd), rather than calling
// kbdir.Configure directly, since the thing under test here is the CLI
// flag/env/precedence plumbing itself (internal/kbdir's own package
// tests already cover the resolution + fs.FS adapter in isolation).
//
// Every test resets kb/sources back to the embedded default afterwards
// so later tests in this package (list_test.go, search_test.go, etc.,
// which assume the embedded KB) aren't affected by execution order.

func resetKBToEmbedded(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := kbdir.Configure(""); err != nil {
			t.Fatalf("reset kbdir.Configure(\"\"): %v", err)
		}
		kbSourceProvenance = kbdir.SourceEmbedded
	})
}

// newFixtureKBDir builds a minimal content-repo-layout directory with
// two real pages, an ingestion source, a prompt, and a template.
func newFixtureKBDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "wiki", "index.md"),
		"---\nid: index\ntitle: Index\ncategory: concepts\n---\n# Index\n\nHome page body.\n")
	write(t, filepath.Join(root, "wiki", "concepts", "widgets.md"),
		"---\nid: concepts/widgets\ntitle: Widgets\ncategory: concepts\nstatus: reviewed\n---\n# Widgets\n\nWidgets are round gadgets.\n")
	write(t, filepath.Join(root, "ingestion", "sources.yaml"),
		"sources:\n"+
			"  - id: widgets-source\n"+
			"    type: files\n"+
			"    repo: acme/widgets\n"+
			"    target_category: concepts\n"+
			"    enumerate:\n"+
			"      kind: files\n"+
			"    template: default.md\n"+
			"    prompt: prompts/general.md\n"+
			"    schedule: weekly\n")
	write(t, filepath.Join(root, "ingestion", "prompts", "general.md"), "# General ingestion prompt\n")
	write(t, filepath.Join(root, "templates", "default.md"), "---\ncategory: concepts\n---\n")
	return root
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func execRoot(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), err
}

func TestKBDirFlag_VersionReportsEmbeddedByDefault(t *testing.T) {
	resetKBToEmbedded(t)

	out, err := execRoot(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if info.KBSource != kbdir.SourceEmbedded {
		t.Errorf("kb_source = %q, want %q", info.KBSource, kbdir.SourceEmbedded)
	}
}

func TestKBDirFlag_VersionReportsDiskPath(t *testing.T) {
	resetKBToEmbedded(t)
	dir := newFixtureKBDir(t)

	out, err := execRoot(t, "--kb-dir", dir, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if want := "disk:" + dir; info.KBSource != want {
		t.Errorf("kb_source = %q, want %q", info.KBSource, want)
	}
	// kb_commit must stay the build-time embedded value — disk content
	// has no build-time pin and must not be conflated with it.
	if info.KBCommit != kbCommit {
		t.Errorf("kb_commit changed to %q under --kb-dir; must stay the build-time embed value %q", info.KBCommit, kbCommit)
	}
}

func TestKBDirFlag_EnvVarConfiguresKBSource(t *testing.T) {
	resetKBToEmbedded(t)
	dir := newFixtureKBDir(t)
	t.Setenv(kbdir.EnvVar, dir)

	out, err := execRoot(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if want := "disk:" + dir; info.KBSource != want {
		t.Errorf("kb_source = %q, want %q", info.KBSource, want)
	}
}

func TestKBDirFlag_FlagWinsOverEnvVar(t *testing.T) {
	resetKBToEmbedded(t)
	flagDir := newFixtureKBDir(t)
	envDir := t.TempDir() // exists but has no content — must NOT be the one used
	t.Setenv(kbdir.EnvVar, envDir)

	out, err := execRoot(t, "--kb-dir", flagDir, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if want := "disk:" + flagDir; info.KBSource != want {
		t.Errorf("kb_source = %q, want the --kb-dir flag path %q (env should lose)", info.KBSource, want)
	}
}

func TestKBDirFlag_NonexistentDirFailsFast(t *testing.T) {
	resetKBToEmbedded(t)
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := execRoot(t, "--kb-dir", dir, "version")
	if err == nil {
		t.Fatal("expected an error for a nonexistent --kb-dir, got nil")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should name the path %q, got: %v", dir, err)
	}
}

func TestKBDirFlag_ListSearchShowServeFromDisk(t *testing.T) {
	resetKBToEmbedded(t)
	dir := newFixtureKBDir(t)

	listOut, err := execRoot(t, "--kb-dir", dir, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, listOut)
	}
	if !strings.Contains(listOut, "concepts/widgets") {
		t.Errorf("list output missing disk page: %s", listOut)
	}

	searchOut, err := execRoot(t, "--kb-dir", dir, "search", "widgets")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, searchOut)
	}
	if !strings.Contains(searchOut, "concepts/widgets") {
		t.Errorf("search output missing disk page: %s", searchOut)
	}

	showOut, err := execRoot(t, "--kb-dir", dir, "show", "concepts/widgets")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, showOut)
	}
	if !strings.Contains(showOut, "Widgets are round gadgets") {
		t.Errorf("show output missing disk page body: %s", showOut)
	}
}
