package contentsource

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvVar is the environment variable consulted for an explicit
// content-source.yaml path when --content-source isn't given.
const EnvVar = "MEERKAT_CONTENT_SOURCE"

// ResolveFlag returns the effective --content-source path given the flag
// value, applying flag > MEERKAT_CONTENT_SOURCE > "" (unset) — mirrors
// internal/kbdir.Resolve for the --kb-dir flag.
func ResolveFlag(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(EnvVar)
}

// LocateRuntime finds the content-source.yaml runtime resolution should
// use. explicit is the result of ResolveFlag (already flag/env
// resolved); pass "" when neither was given.
//
// Discovery order once explicit is empty: os.UserConfigDir()/meerkat/
// content-source.yaml, then ./content-source.yaml in the working
// directory. Returns path == "" (not an error) when explicit is empty
// and neither of those exist — the caller should then fall back to
// embedded content.
//
// An explicit path that does not exist IS an error: the operator named
// it, so silently ignoring it would be as confusing as --kb-dir pointing
// at a directory that doesn't exist.
func LocateRuntime(explicit string) (path string, err error) {
	if explicit != "" {
		if !fileExists(explicit) {
			return "", fmt.Errorf("--content-source %q does not exist", explicit)
		}
		return explicit, nil
	}
	if dir, uerr := os.UserConfigDir(); uerr == nil {
		p := filepath.Join(dir, "meerkat", ConfigFile)
		if fileExists(p) {
			return p, nil
		}
	}
	if fileExists(ConfigFile) {
		return ConfigFile, nil
	}
	return "", nil
}

// LoadFile reads + validates a content-source.yaml at an exact path —
// unlike Load, which resolves ConfigFile under a repo root and treats a
// missing file as type: none. A missing file here is a plain error:
// callers (ResolveRuntime, via LocateRuntime) only ever pass a path
// they've already confirmed exists.
func LoadFile(path string) (Config, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: path is an operator-supplied config location (--content-source/MEERKAT_CONTENT_SOURCE, the user config dir, or ./content-source.yaml) — not attacker-influenced input.
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	return parseConfig(body, path)
}

// RuntimeContent is the outcome of ResolveRuntime: what to serve, and
// how it was configured. Dir == "" means "serve the embedded build" —
// either no content-source.yaml was found, or it explicitly says
// type: none. Source is the zero Source in that case; otherwise it's
// the fully-resolved (defaulted, validated) config, mainly for callers
// that need Source.Layout or — for TypeURL — the URL/digest for
// provenance reporting (see URLProvenance).
type RuntimeContent struct {
	Dir    string
	Source Source
}

// ResolveRuntime implements steps 2-5 of the documented runtime content
// resolution order (step 1, --kb-dir/MEERKAT_KB_DIR, is resolved by the
// caller — internal/cli, via internal/kbdir.Resolve — and takes priority
// over everything here: ResolveRuntime is only consulted once that's
// confirmed empty):
//
//  2. contentSourceFlag (or MEERKAT_CONTENT_SOURCE if that's empty) — an
//     explicit content-source.yaml path.
//  3. os.UserConfigDir()/meerkat/content-source.yaml
//  4. ./content-source.yaml (the working directory)
//  5. none of the above exist -> RuntimeContent{} (embedded fallback)
//
// type: none (explicit, or implied by no config being found at all)
// resolves to the same embedded fallback. type: local resolves Dir
// against the content-source.yaml's own directory when Path is relative
// (mirroring Load's repoRoot-relative behaviour for build-time local
// sources). type: url is fetched/verified/extracted/cached via
// FetchURL. type: git and type: submodule — valid at build time (Load,
// Sync) — are rejected here with an error naming the type as build-time
// only: they need git and a working tree, neither of which a shipped
// runtime binary can assume.
func ResolveRuntime(contentSourceFlag string) (RuntimeContent, error) {
	explicit := ResolveFlag(contentSourceFlag)
	path, err := LocateRuntime(explicit)
	if err != nil {
		return RuntimeContent{}, err
	}
	if path == "" {
		return RuntimeContent{}, nil
	}
	cfg, err := LoadFile(path)
	if err != nil {
		return RuntimeContent{}, fmt.Errorf("content-source.yaml (%s): %w", path, err)
	}
	src := cfg.Content
	switch src.Type {
	case TypeNone:
		return RuntimeContent{}, nil
	case TypeLocal:
		dir := src.Path
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(path), dir)
		}
		return RuntimeContent{Dir: dir, Source: src}, nil
	case TypeURL:
		dir, ferr := FetchURL(src)
		if ferr != nil {
			return RuntimeContent{}, fmt.Errorf("content-source.yaml (%s): %w", path, ferr)
		}
		return RuntimeContent{Dir: dir, Source: src}, nil
	case TypeGit, TypeSubmodule:
		return RuntimeContent{}, fmt.Errorf(
			"content-source.yaml (%s): type %q is build-time only (it needs git and a working tree) — "+
				"not supported at runtime; use `make sync` to embed it at build time instead, or switch "+
				"to type: url or type: local for a runtime-resolved source", path, src.Type)
	default:
		// Unreachable: LoadFile's Validate call already rejects any type
		// not in {none, local, git, submodule, url}. Kept as an explicit
		// safety net — an unrecognised type must never silently fall
		// through to the embedded-content default — in case that
		// invariant is ever loosened.
		return RuntimeContent{}, fmt.Errorf("content-source.yaml (%s): unsupported type %q", path, src.Type)
	}
}

// URLProvenance formats the kb_source string `mk version` reports for a
// type: url content source: "url:<url>@<sha256 first 12 hex chars>".
//
// Unlike "disk:<path>" (internal/kbdir.Configure/ConfigureLayout's
// provenance for --kb-dir / MEERKAT_KB_DIR / a type: local source — an
// arbitrary, unverified directory), a url: source IS content-verified:
// FetchURL never extracts or caches an archive unless its sha256 matches
// src.SHA256 exactly, so the digest in this string is a real, checked
// property of the content actually being served, not just a label.
func URLProvenance(src Source) string {
	digest := strings.ToLower(src.SHA256)
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return fmt.Sprintf("url:%s@%s", src.URL, digest)
}
