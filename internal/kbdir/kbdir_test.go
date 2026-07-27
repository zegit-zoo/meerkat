package kbdir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

// These tests exercise Configure end-to-end through the real kb/sources
// APIs (kb.List/Load, sources.All/Prompt/Template) against a hand-built
// content-repo-layout directory, mirroring the injection-test style used
// by internal/search/inject_test.go — but here the "injection" is a real
// directory on disk rather than an in-memory fixture, since that's the
// actual mechanism under test.
//
// Configure mutates package-level state in kb/sources; every test that
// calls it restores the embedded default via t.Cleanup so later tests
// (in this file or elsewhere in the package) aren't affected by
// execution order.

func resetToEmbedded(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := Configure(""); err != nil {
			t.Fatalf("reset Configure(\"\"): %v", err)
		}
	})
}

// newContentRepo builds a minimal content-repo-layout directory (the
// layout content-source.yaml describes and `mk ingest` writes into):
//
//	wiki/index.md
//	wiki/concepts/widgets.md
//	ingestion/sources.yaml
//	ingestion/prompts/general.md
//	templates/default.md
func newContentRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "wiki", "index.md"),
		"---\nid: index\ntitle: Index\ncategory: concepts\n---\n# Index\n\nHome page body.\n")
	write(t, filepath.Join(root, "wiki", "concepts", "widgets.md"),
		"---\nid: concepts/widgets\ntitle: Widgets\ncategory: concepts\nstatus: reviewed\n---\n# Widgets\n\nWidgets are round.\n")
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

func TestConfigure_DiskTakesPrecedenceOverEmbed(t *testing.T) {
	resetToEmbedded(t)
	root := newContentRepo(t)

	source, err := Configure(root)
	if err != nil {
		t.Fatalf("Configure(%q): %v", root, err)
	}
	if want := "disk:" + root; source != want {
		t.Errorf("source = %q, want %q", source, want)
	}

	pages, err := kb.List()
	if err != nil {
		t.Fatalf("kb.List: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("kb.List returned %d pages, want 2: %+v", len(pages), pages)
	}

	page, err := kb.Load("concepts/widgets")
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}
	if page.Title != "Widgets" {
		t.Errorf("Title = %q, want Widgets", page.Title)
	}
	if !strings.Contains(page.Body, "Widgets are round") {
		t.Errorf("Body missing expected text: %q", page.Body)
	}
	if page.Front.Status != "reviewed" {
		t.Errorf("Status = %q, want reviewed", page.Front.Status)
	}
}

func TestConfigure_EmptyFallsBackToEmbed(t *testing.T) {
	resetToEmbedded(t)
	root := newContentRepo(t)

	if _, err := Configure(root); err != nil {
		t.Fatalf("Configure(%q): %v", root, err)
	}
	diskPages, err := kb.List()
	if err != nil {
		t.Fatalf("kb.List (disk): %v", err)
	}
	if len(diskPages) == 0 {
		t.Fatal("expected disk fixture pages before falling back")
	}

	source, err := Configure("")
	if err != nil {
		t.Fatalf("Configure(\"\"): %v", err)
	}
	if source != SourceEmbedded {
		t.Errorf("source = %q, want %q", source, SourceEmbedded)
	}

	embedPages, err := kb.List()
	if err != nil {
		t.Fatalf("kb.List (embedded): %v", err)
	}
	for _, p := range embedPages {
		if p.ID == "concepts/widgets" {
			t.Errorf("fallback still serving disk fixture page %q", p.ID)
		}
	}
}

func TestConfigure_DegradesToEmptyForMissingSubdirectory(t *testing.T) {
	resetToEmbedded(t)
	root := t.TempDir()
	// Only ingestion/ is populated; wiki/ and templates/ don't exist at
	// all. This is the "freshly created / partially populated --kb-dir"
	// case the design calls out as degrade-not-fail.
	write(t, filepath.Join(root, "ingestion", "sources.yaml"), "sources: []\n")

	source, err := Configure(root)
	if err != nil {
		t.Fatalf("Configure(%q): %v", root, err)
	}
	if want := "disk:" + root; source != want {
		t.Errorf("source = %q, want %q", source, want)
	}

	pages, err := kb.List()
	if err != nil {
		t.Fatalf("kb.List should degrade to empty, not error: %v", err)
	}
	if len(pages) != 0 {
		t.Errorf("kb.List returned %d pages, want 0", len(pages))
	}

	all, err := sources.All()
	if err != nil {
		t.Fatalf("sources.All: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("sources.All returned %d sources, want 0", len(all))
	}

	_, err = kb.Load("concepts/widgets")
	if !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("kb.Load on missing subdir = %v, want kb.ErrNotFound", err)
	}
}

func TestConfigure_NonexistentDirFailsFast(t *testing.T) {
	resetToEmbedded(t)
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := Configure(dir)
	if err == nil {
		t.Fatal("expected an error for a nonexistent --kb-dir, got nil")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should name the path %q, got: %v", dir, err)
	}
}

func TestConfigure_FileInsteadOfDirIsError(t *testing.T) {
	resetToEmbedded(t)
	f := filepath.Join(t.TempDir(), "notadir")
	write(t, f, "x")

	_, err := Configure(f)
	if err == nil {
		t.Fatal("expected an error when --kb-dir points at a file, got nil")
	}
}

func TestConfigure_SourcesPromptAndTemplateResolveFromDisk(t *testing.T) {
	resetToEmbedded(t)
	root := newContentRepo(t)

	if _, err := Configure(root); err != nil {
		t.Fatalf("Configure(%q): %v", root, err)
	}

	all, err := sources.All()
	if err != nil {
		t.Fatalf("sources.All: %v", err)
	}
	if len(all) != 1 || all[0].ID != "widgets-source" {
		t.Fatalf("sources.All = %+v, want one source 'widgets-source'", all)
	}

	prompt, err := sources.Prompt(all[0].Prompt)
	if err != nil {
		t.Fatalf("sources.Prompt: %v", err)
	}
	if !strings.Contains(prompt, "General ingestion prompt") {
		t.Errorf("prompt content = %q, missing expected text", prompt)
	}

	tpl, err := sources.Template(all[0].Template)
	if err != nil {
		t.Fatalf("sources.Template: %v", err)
	}
	if !strings.Contains(tpl, "category: concepts") {
		t.Errorf("template content = %q, missing expected text", tpl)
	}
}

func TestResolve_FlagWinsOverEnv(t *testing.T) {
	t.Setenv(EnvVar, "/env/path")
	if got := Resolve("/flag/path"); got != "/flag/path" {
		t.Errorf("Resolve = %q, want /flag/path", got)
	}
}

func TestResolve_FallsBackToEnv(t *testing.T) {
	t.Setenv(EnvVar, "/env/path")
	if got := Resolve(""); got != "/env/path" {
		t.Errorf("Resolve = %q, want /env/path", got)
	}
}

func TestResolve_EmptyWhenNeitherSet(t *testing.T) {
	t.Setenv(EnvVar, "")
	if got := Resolve(""); got != "" {
		t.Errorf("Resolve = %q, want empty", got)
	}
}

func TestFS_MapsEmbedStylePathsOntoContentRepoLayout(t *testing.T) {
	root := newContentRepo(t)
	fsys, err := FS(root)
	if err != nil {
		t.Fatalf("FS(%q): %v", root, err)
	}

	body, err := fs.ReadFile(fsys, "content/index.md")
	if err != nil {
		t.Fatalf("read content/index.md: %v", err)
	}
	if !strings.Contains(string(body), "Home page body") {
		t.Errorf("unexpected content/index.md body: %q", body)
	}

	if _, err := fs.ReadFile(fsys, "etc/sources.yaml"); err != nil {
		t.Errorf("read etc/sources.yaml: %v", err)
	}
	if _, err := fs.ReadFile(fsys, "etc/prompts/general.md"); err != nil {
		t.Errorf("read etc/prompts/general.md: %v", err)
	}
	if _, err := fs.ReadFile(fsys, "etc/templates/default.md"); err != nil {
		t.Errorf("read etc/templates/default.md: %v", err)
	}

	// A path outside the mapped prefixes must look like a normal
	// missing file, not panic or otherwise misbehave.
	if _, err := fsys.Open("something/unmapped"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(unmapped) = %v, want fs.ErrNotExist", err)
	}
}
