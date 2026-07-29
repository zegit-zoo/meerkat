// Package kbdir resolves meerkat's runtime knowledge-base directory —
// the --kb-dir flag / MEERKAT_KB_DIR env var, or a directory resolved
// from content-source.yaml by internal/cli via internal/contentsource —
// and, when one is configured, adapts a content-repo-layout directory
// on disk onto the embed-style paths ("content/...", "etc/...") that
// internal/kb and internal/sources already know how to read. That
// means kb.List/Load and sources.All/Prompt/Template serve disk content
// unchanged — no caller of those packages needs to change.
//
// The runtime directory uses the CONTENT-REPO layout — the layout
// content-source.yaml describes and `mk ingest` writes into — not the
// internal embed layout. That's what makes ingest output visible with
// no rebuild. The mapping is parameterised by a contentsource.Layout
// (ConfigureLayout / FSLayout); the documented defaults below apply
// whenever a layout field is left empty, which includes every --kb-dir /
// MEERKAT_KB_DIR call (Configure / FS always pass the zero Layout{} —
// --kb-dir has no way to carry a custom layout of its own, so it always
// gets the defaults, exactly as before this package read layout: at
// all):
//
//	<kb-dir>/wiki/                    -> content/        (kb package)
//	<kb-dir>/ingestion/sources.yaml   -> etc/sources.yaml (sources package)
//	<kb-dir>/ingestion/prompts/       -> etc/prompts/     (sources package)
//	<kb-dir>/templates/               -> etc/templates/   (sources package)
package kbdir

import (
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

// EnvVar is the environment variable consulted when --kb-dir isn't given.
const EnvVar = "MEERKAT_KB_DIR"

// SourceEmbedded is the kb_source provenance value reported by `mk
// version` when no runtime directory is configured.
const SourceEmbedded = "embedded"

// Resolve returns the effective kb-dir path given the --kb-dir flag
// value, applying the documented priority: flag, then MEERKAT_KB_DIR,
// then "" (unset — callers should serve the embedded content).
func Resolve(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(EnvVar)
}

// Configure applies dir (as returned by Resolve) to internal/kb and
// internal/sources, redirecting them to read from dir instead of their
// embedded content, using the default content-repo layout. It returns
// the kb_source provenance string `mk version` reports: SourceEmbedded
// when dir is empty, "disk:<dir>" otherwise.
//
// This is the --kb-dir / MEERKAT_KB_DIR entry point specifically — it
// always uses the default layout, since that flag has no way to carry a
// custom one. For a caller-supplied layout (a type: local or type: url
// content-source.yaml source's layout: block), see ConfigureLayout.
func Configure(dir string) (source string, err error) {
	return ConfigureLayout(dir, contentsource.Layout{})
}

// ConfigureLayout is Configure generalised to a caller-supplied layout:
// any field left empty in layout takes the same documented default
// Configure always uses (see contentsource.MergeLayout).
//
// dir == "" is not an error: it means no content directory was
// configured (no --kb-dir/MEERKAT_KB_DIR, and no content-source.yaml
// resolved to a directory either), so kb/sources keep reading their
// embedded content (the fallback) — layout is meaningless in this case
// and ignored. A non-empty dir that does not exist at all IS an error —
// silently falling back to the embed would be baffling for an operator
// who explicitly configured a content directory. A dir that exists but
// is missing some or all of the expected wiki/ingestion/templates
// subdirectories is NOT an error: kb.List/sources.All already degrade a
// missing subtree to an empty result (same as the embed's zero-content
// default), so a partially-populated or freshly-created directory just
// serves what it has.
func ConfigureLayout(dir string, layout contentsource.Layout) (source string, err error) {
	if dir == "" {
		kb.UseFS(nil)
		sources.UseFS(nil)
		return SourceEmbedded, nil
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", fmt.Errorf("content directory %q does not exist", dir)
		}
		return "", fmt.Errorf("content directory %q: %w", dir, statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("content directory %q is not a directory", dir)
	}
	fsys, err := FSLayout(dir, layout)
	if err != nil {
		return "", err
	}
	kb.UseFS(fsys)
	sources.UseFS(fsys)
	return "disk:" + dir, nil
}

// FS adapts dir (in content-repo layout, using the default layout) onto
// the embed-style paths kb/sources expect. Exported mainly for tests
// that want the adapter without going through Configure's global-state
// side effects.
func FS(dir string) (fs.FS, error) { return FSLayout(dir, contentsource.Layout{}) }

// FSLayout is FS generalised to a caller-supplied layout: any field left
// empty in layout takes the same documented default FS uses (see
// contentsource.MergeLayout).
func FSLayout(dir string, layout contentsource.Layout) (fs.FS, error) {
	// SECURITY: os.OpenRoot, not os.DirFS. os.DirFS only rejects ".." in
	// the *path argument* — it does not stop a symlink stored *inside*
	// the tree from pointing outside it, which the os.DirFS doc comment
	// calls out explicitly and recommends Root.FS for. That distinction
	// is load-bearing here: the content directory is routinely a
	// checked-out content repo (git stores symlinks verbatim), an
	// extracted archive, or an ingest-populated tree, and its contents
	// are served to MCP clients and over HTTP. Without os.Root, a single
	// `wiki/leak.md -> /etc/passwd` becomes arbitrary file read via `mk
	// show`, and remote arbitrary file read on an exposed server. os.Root
	// resolves each path component and refuses to traverse out of the
	// tree.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open content directory %q: %w", dir, err)
	}
	// root is deliberately retained (never closed) for the process
	// lifetime: it holds the directory handle that backs every later
	// read, and os.Root has a finalizer that would close it if the only
	// reference were the fs.FS derived from it.
	return adapter{root: root.FS(), keepalive: root, layout: contentsource.MergeLayout(layout)}, nil
}

// adapter maps embed-style paths ("content/...", "etc/...") onto the
// content-repo layout within root, per layout.
type adapter struct {
	root      fs.FS
	keepalive *os.Root
	layout    contentsource.Layout // always fully merged/defaulted — see FSLayout.
}

func (a adapter) Open(name string) (fs.File, error) {
	mapped, ok := a.mapPath(name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return a.root.Open(mapped)
}

// mapPath translates a single embed-style path to its content-repo
// counterpart per a.layout (see the package doc comment for the default
// layout table).
func (a adapter) mapPath(name string) (string, bool) {
	switch {
	case name == "content":
		return a.layout.Wiki, true
	case strings.HasPrefix(name, "content/"):
		return a.layout.Wiki + "/" + strings.TrimPrefix(name, "content/"), true
	case name == "etc/sources.yaml":
		return a.layout.Sources, true
	case name == "etc/prompts":
		return a.layout.Prompts, true
	case strings.HasPrefix(name, "etc/prompts/"):
		return a.layout.Prompts + "/" + strings.TrimPrefix(name, "etc/prompts/"), true
	case name == "etc/templates":
		return a.layout.Templates, true
	case strings.HasPrefix(name, "etc/templates/"):
		return a.layout.Templates + "/" + strings.TrimPrefix(name, "etc/templates/"), true
	default:
		return "", false
	}
}
