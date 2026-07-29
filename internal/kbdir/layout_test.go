package kbdir

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

// newCustomLayoutRepo builds a content-repo directory using a
// deliberately NON-default layout (the shape a content-source.yaml's
// layout: block, or a type: url archive with its own layout, might
// describe), so tests can prove the mapping is actually parameterised
// rather than still secretly hardcoded to wiki/ingestion/templates.
func newCustomLayoutRepo(t *testing.T) (root string, layout contentsource.Layout) {
	t.Helper()
	root = t.TempDir()
	write(t, filepath.Join(root, "docs", "index.md"),
		"---\nid: index\ntitle: Index\n---\n# Index\n\ncustom layout home page.\n")
	write(t, filepath.Join(root, "docs", "concepts", "widgets.md"),
		"---\nid: concepts/widgets\ntitle: Widgets\n---\n# Widgets\n\ncustom layout widgets.\n")
	write(t, filepath.Join(root, "meta", "registry.yaml"),
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
	write(t, filepath.Join(root, "meta", "prompts", "general.md"), "# Custom layout prompt\n")
	write(t, filepath.Join(root, "meta", "templates", "default.md"), "---\ncategory: concepts\n---\n")
	layout = contentsource.Layout{
		Wiki:      "docs",
		Sources:   "meta/registry.yaml",
		Prompts:   "meta/prompts",
		Templates: "meta/templates",
	}
	return root, layout
}

func TestConfigureLayout_CustomLayoutResolvesThroughKBAndSources(t *testing.T) {
	resetToEmbedded(t)
	root, layout := newCustomLayoutRepo(t)

	source, err := ConfigureLayout(root, layout)
	if err != nil {
		t.Fatalf("ConfigureLayout(%q, %+v): %v", root, layout, err)
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
	if !strings.Contains(page.Body, "custom layout widgets") {
		t.Errorf("Body missing expected text: %q", page.Body)
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
	if !strings.Contains(prompt, "Custom layout prompt") {
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

// TestConfigureLayout_PartialLayoutMergesDefaults: overriding only Wiki
// must still resolve Sources/Prompts/Templates via the documented
// defaults (ingestion/sources.yaml etc.), not silently serve nothing for
// the fields left unset.
//
// ingestion/sources.yaml here deliberately contains a real entry (not an
// empty list): sources.All() degrades a missing/unreadable registry to
// an empty result with no error (see sources.All's doc comment), which
// would make an empty-list fixture indistinguishable from "the default
// path was never actually consulted" — a real entry is the only way to
// prove the default Sources path resolved and was read.
func TestConfigureLayout_PartialLayoutMergesDefaults(t *testing.T) {
	resetToEmbedded(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "docs", "index.md"), "---\nid: index\n---\n# Index\n\nhome\n")
	write(t, filepath.Join(root, "ingestion", "sources.yaml"),
		"sources:\n"+
			"  - id: default-path-source\n"+
			"    type: files\n"+
			"    repo: acme/widgets\n"+
			"    target_category: concepts\n"+
			"    enumerate:\n"+
			"      kind: files\n")

	source, err := ConfigureLayout(root, contentsource.Layout{Wiki: "docs"})
	if err != nil {
		t.Fatalf("ConfigureLayout: %v", err)
	}
	if want := "disk:" + root; source != want {
		t.Errorf("source = %q, want %q", source, want)
	}

	pages, err := kb.List()
	if err != nil {
		t.Fatalf("kb.List: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("kb.List returned %d pages, want 1 (from the custom 'docs' wiki dir)", len(pages))
	}
	all, err := sources.All()
	if err != nil {
		t.Fatalf("sources.All: %v", err)
	}
	if len(all) != 1 || all[0].ID != "default-path-source" {
		t.Fatalf("sources.All = %+v, want one source 'default-path-source' (proves the unset Layout.Sources field defaulted to ingestion/sources.yaml)", all)
	}
}

// TestConfigureLayout_ZeroLayoutMatchesConfigure: ConfigureLayout(dir,
// contentsource.Layout{}) must behave identically to Configure(dir) —
// Configure is defined purely as a thin wrapper around this, and every
// --kb-dir/MEERKAT_KB_DIR caller depends on that equivalence continuing
// to hold.
func TestConfigureLayout_ZeroLayoutMatchesConfigure(t *testing.T) {
	resetToEmbedded(t)
	root := newContentRepo(t)

	viaConfigure, err := Configure(root)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pagesViaConfigure, err := kb.List()
	if err != nil {
		t.Fatal(err)
	}

	resetToEmbedded(t)
	viaLayout, err := ConfigureLayout(root, contentsource.Layout{})
	if err != nil {
		t.Fatalf("ConfigureLayout: %v", err)
	}
	pagesViaLayout, err := kb.List()
	if err != nil {
		t.Fatal(err)
	}

	if viaConfigure != viaLayout {
		t.Errorf("source = %q via Configure, %q via ConfigureLayout(zero layout)", viaConfigure, viaLayout)
	}
	if len(pagesViaConfigure) != len(pagesViaLayout) {
		t.Errorf("page count differs: Configure=%d ConfigureLayout=%d", len(pagesViaConfigure), len(pagesViaLayout))
	}
}

func TestFSLayout_CustomLayoutMapsEmbedStylePaths(t *testing.T) {
	root, layout := newCustomLayoutRepo(t)
	fsys, err := FSLayout(root, layout)
	if err != nil {
		t.Fatalf("FSLayout(%q, %+v): %v", root, layout, err)
	}

	body, err := fs.ReadFile(fsys, "content/index.md")
	if err != nil {
		t.Fatalf("read content/index.md: %v", err)
	}
	if !strings.Contains(string(body), "custom layout home page") {
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
}

// TestFSLayout_CustomLayoutSymlinkEscapeStillBlocked proves the os.Root
// containment kbdir relies on doesn't care about which layout is in
// play — the security property from security_test.go's
// TestAdapt_SymlinkEscapeIsBlocked must hold identically for a custom
// layout, not just the default one.
func TestFSLayout_CustomLayoutSymlinkEscapeStillBlocked(t *testing.T) {
	root, layout := newCustomLayoutRepo(t)
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "passwd")
	write(t, secretFile, "SECRET-CUSTOM-LAYOUT-BYTES")
	if err := os.Symlink(secretFile, filepath.Join(root, "docs", "leak.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	fsys, err := FSLayout(root, layout)
	if err != nil {
		t.Fatalf("FSLayout: %v", err)
	}
	if body, err := fs.ReadFile(fsys, "content/leak.md"); err == nil || strings.Contains(string(body), "SECRET-CUSTOM-LAYOUT-BYTES") {
		t.Fatalf("content/leak.md = body=%q err=%v, want an error and no leaked bytes", body, err)
	}
	// The legitimate page must still resolve.
	if body, err := fs.ReadFile(fsys, "content/index.md"); err != nil || !strings.Contains(string(body), "custom layout home page") {
		t.Errorf("legitimate page failed to load through a custom layout after the symlink defense: body=%q err=%v", body, err)
	}
}
