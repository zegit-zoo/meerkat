// Package kbdir resolves meerkat's runtime knowledge-base directory
// (the --kb-dir flag / MEERKAT_KB_DIR env var) and, when one is
// configured, adapts a content-repo-layout directory on disk onto the
// embed-style paths ("content/...", "etc/...") that internal/kb and
// internal/sources already know how to read. That means kb.List/Load
// and sources.All/Prompt/Template serve disk content unchanged — no
// caller of those packages needs to change.
//
// The runtime directory uses the CONTENT-REPO layout — the layout
// content-source.yaml describes and `mk ingest` writes into — not the
// internal embed layout. That's what makes ingest output visible with
// no rebuild:
//
//	<kb-dir>/wiki/                    -> content/        (kb package)
//	<kb-dir>/ingestion/sources.yaml   -> etc/sources.yaml (sources package)
//	<kb-dir>/ingestion/prompts/       -> etc/prompts/     (sources package)
//	<kb-dir>/templates/               -> etc/templates/   (sources package)
//
// TODO(layout): content-source.yaml's `layout:` block
// (internal/contentsource.Layout) is intentionally NOT consulted here —
// this package only supports the documented defaults above (mirrored,
// not imported, from internal/contentsource/config.go's defaultLayout;
// see config.go:70-73). Reading a custom layout at runtime is a
// follow-up; until then a --kb-dir with a non-default layout will look
// empty rather than erroring.
package kbdir

import (
	"fmt"
	"io/fs"
	"os"
	"strings"

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
// embedded content, and returns the kb_source provenance string `mk
// version` reports: SourceEmbedded when dir is empty, "disk:<dir>"
// otherwise.
//
// dir == "" is not an error: it means no --kb-dir/MEERKAT_KB_DIR was
// given, so kb/sources keep reading their embedded content (the
// fallback). A dir that does not exist at all IS an error — silently
// falling back to the embed would be baffling for an operator who
// explicitly pointed --kb-dir somewhere. A dir that exists but is
// missing some or all of the expected wiki/ingestion/templates
// subdirectories is NOT an error: kb.List/sources.All already degrade
// a missing subtree to an empty result (same as the embed's
// zero-content default), so a partially-populated or freshly-created
// --kb-dir just serves what it has.
func Configure(dir string) (source string, err error) {
	if dir == "" {
		kb.UseFS(nil)
		sources.UseFS(nil)
		return SourceEmbedded, nil
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", fmt.Errorf("--kb-dir %q does not exist", dir)
		}
		return "", fmt.Errorf("--kb-dir %q: %w", dir, statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--kb-dir %q is not a directory", dir)
	}
	fsys, err := adapt(dir)
	if err != nil {
		return "", err
	}
	kb.UseFS(fsys)
	sources.UseFS(fsys)
	return "disk:" + dir, nil
}

// FS adapts dir (in content-repo layout) onto the embed-style paths
// kb/sources expect. Exported mainly for tests that want the adapter
// without going through Configure's global-state side effects.
func FS(dir string) (fs.FS, error) { return adapt(dir) }

func adapt(dir string) (fs.FS, error) {
	// SECURITY: os.OpenRoot, not os.DirFS. os.DirFS only rejects ".." in
	// the *path argument* — it does not stop a symlink stored *inside*
	// the tree from pointing outside it, which the os.DirFS doc comment
	// calls out explicitly and recommends Root.FS for. That distinction
	// is load-bearing here: the kb-dir is routinely a checked-out content
	// repo (git stores symlinks verbatim) or an extracted archive, and
	// its contents are served to MCP clients and over HTTP. Without
	// os.Root, a single `wiki/leak.md -> /etc/passwd` becomes arbitrary
	// file read via `mk show`, and remote arbitrary file read on an
	// exposed server. os.Root resolves each path component and refuses
	// to traverse out of the tree.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("--kb-dir %q: %w", dir, err)
	}
	// root is deliberately retained (never closed) for the process
	// lifetime: it holds the directory handle that backs every later
	// read, and os.Root has a finalizer that would close it if the only
	// reference were the fs.FS derived from it.
	return adapter{root: root.FS(), keepalive: root}, nil
}

// adapter maps embed-style paths ("content/...", "etc/...") onto the
// content-repo layout within root.
type adapter struct {
	root      fs.FS
	keepalive *os.Root
}

func (a adapter) Open(name string) (fs.File, error) {
	mapped, ok := mapPath(name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return a.root.Open(mapped)
}

// mapPath translates a single embed-style path to its content-repo
// counterpart (see the package doc comment for the full layout table).
func mapPath(name string) (string, bool) {
	switch {
	case name == "content":
		return "wiki", true
	case strings.HasPrefix(name, "content/"):
		return "wiki/" + strings.TrimPrefix(name, "content/"), true
	case name == "etc/sources.yaml":
		return "ingestion/sources.yaml", true
	case name == "etc/prompts":
		return "ingestion/prompts", true
	case strings.HasPrefix(name, "etc/prompts/"):
		return "ingestion/prompts/" + strings.TrimPrefix(name, "etc/prompts/"), true
	case name == "etc/templates":
		return "templates", true
	case strings.HasPrefix(name, "etc/templates/"):
		return "templates/" + strings.TrimPrefix(name, "etc/templates/"), true
	default:
		return "", false
	}
}
