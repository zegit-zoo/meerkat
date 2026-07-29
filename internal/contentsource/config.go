// Package contentsource resolves meerkat's knowledge-base content source
// from a declarative content-source.yaml. It serves two purposes:
//
//   - Build time (Load, Sync, ResolveWorkingCopy; invoked by `make sync` /
//     the GoReleaser before-hook): populates the package-local embed
//     directories (internal/kb/content, internal/sources/etc) baked into
//     the binary by go:embed. Supports type: none|local|git|submodule.
//
//   - Run time (LoadFile, ResolveRuntime, FetchURL; invoked by
//     internal/cli's PersistentPreRunE, once --kb-dir/MEERKAT_KB_DIR is
//     confirmed unset): resolves a content-source.yaml discovered via
//     --content-source/MEERKAT_CONTENT_SOURCE, the user config dir, or
//     the working directory, to a directory internal/kbdir can serve.
//     Supports type: none|local|url — type: git and type: submodule are
//     build-time only (they need git and a working tree) and are
//     rejected with an actionable error at runtime.
package contentsource

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFile is the repo-root config filename.
const ConfigFile = "content-source.yaml"

// Source type values.
const (
	TypeNone      = "none"
	TypeLocal     = "local"
	TypeGit       = "git"
	TypeSubmodule = "submodule"
	// TypeURL fetches a .tar.gz content archive over HTTPS, verifies it
	// against a mandatory sha256 digest, and caches the extracted result
	// keyed by that digest. Unlike TypeGit/TypeSubmodule, it needs no git
	// or working tree, so — unlike them — it is supported at runtime as
	// well as at build time. See FetchURL and ResolveRuntime.
	TypeURL = "url"
)

// Config is the top-level content-source.yaml document.
type Config struct {
	Content Source `yaml:"content"`
}

// Source declares a single content source (build-time or runtime — see
// the package doc comment).
type Source struct {
	Type string `yaml:"type"` // none | local | git | submodule | url

	// type: local
	Path string `yaml:"path,omitempty"` // content root, relative to repo root (build time) or the config file's directory (run time), or absolute

	// type: git — build-time only.
	Repo string `yaml:"repo,omitempty"` // owner/repo slug, or a full clone URL
	// Host selects the clone domain: github | gitlab. For a private GitHub
	// repo, a cached `gh` token is used automatically. GitLab (or any other
	// host) has no credential borrowing — configure private access through
	// normal git credentials, or give Repo a full clone URL / SSH spec.
	Host string `yaml:"host,omitempty"`
	Ref  string `yaml:"ref,omitempty"` // tag, branch, or commit SHA (what the build embeds)

	// Branch is the push target for `mk ingest` write-back. Optional: when
	// empty, ingest uses Ref if it looks like a branch, else the remote
	// default. Lets you embed a pinned tag (ref) but ingest to a branch.
	Branch string `yaml:"branch,omitempty"`

	// type: submodule — build-time only.
	Submodule string `yaml:"submodule,omitempty"` // submodule path (e.g. "kb")

	// type: url — runtime-capable (see FetchURL). URL must be an
	// https:// URL; SHA256 is REQUIRED (not merely recommended — see
	// Validate) since it is what makes fetched content verifiable and
	// what the on-disk cache is keyed on.
	URL    string `yaml:"url,omitempty"`
	SHA256 string `yaml:"sha256,omitempty"`

	// Layout maps artifacts to their location WITHIN the resolved source.
	Layout Layout `yaml:"layout,omitempty"`
}

// Layout locates each embeddable artifact inside the resolved source root.
type Layout struct {
	Wiki      string `yaml:"wiki,omitempty"`      // markdown pages   -> internal/kb/content/
	Sources   string `yaml:"sources,omitempty"`   // source registry  -> internal/sources/etc/sources.yaml
	Prompts   string `yaml:"prompts,omitempty"`   // per-source prompts -> internal/sources/etc/prompts/
	Templates string `yaml:"templates,omitempty"` // page templates   -> internal/sources/etc/templates/
}

func defaultLayout() Layout {
	return Layout{
		Wiki:      "wiki",
		Sources:   "ingestion/sources.yaml",
		Prompts:   "ingestion/prompts",
		Templates: "templates",
	}
}

// Load reads content-source.yaml from repoRoot, applies layout defaults, and
// validates. A missing file is not an error: it yields a "none" source so the
// build proceeds with the committed embed placeholders (the OSS default).
//
// Build-time only (repoRoot-relative). For the runtime equivalent — an
// exact file path, with a missing file treated as "keep looking" rather
// than "type: none" — see LoadFile / ResolveRuntime.
func Load(repoRoot string) (Config, error) {
	body, err := os.ReadFile(filepath.Join(repoRoot, ConfigFile)) //nolint:gosec // G304: build-time tool reading its own config from the repo root.
	if errors.Is(err, os.ErrNotExist) {
		return Config{Content: Source{Type: TypeNone}}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", ConfigFile, err)
	}
	return parseConfig(body, ConfigFile)
}

// parseConfig unmarshals + defaults + validates the bytes of a
// content-source.yaml already read from disk (by Load or LoadFile).
// displayPath is used only in error messages.
func parseConfig(body []byte, displayPath string) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", displayPath, err)
	}
	if cfg.Content.Type == "" {
		cfg.Content.Type = TypeNone
	}
	cfg.Content.Layout = MergeLayout(cfg.Content.Layout)
	if err := cfg.Content.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// MergeLayout fills any empty field of l with the documented default for
// that artifact, leaving explicitly-set fields untouched. Exported so
// internal/kbdir can merge a layout (or the zero Layout{} used for
// --kb-dir / MEERKAT_KB_DIR, which has no way to carry a custom layout of
// its own) the same way Load/LoadFile do.
func MergeLayout(l Layout) Layout {
	d := defaultLayout()
	if l.Wiki == "" {
		l.Wiki = d.Wiki
	}
	if l.Sources == "" {
		l.Sources = d.Sources
	}
	if l.Prompts == "" {
		l.Prompts = d.Prompts
	}
	if l.Templates == "" {
		l.Templates = d.Templates
	}
	return l
}

// Validate checks the source for the required fields of its type.
func (s Source) Validate() error {
	switch s.Type {
	case TypeNone:
		return nil
	case TypeLocal:
		if s.Path == "" {
			return errors.New("content.path is required for type: local")
		}
	case TypeGit:
		if s.Repo == "" {
			return errors.New("content.repo is required for type: git")
		}
		if s.Ref == "" {
			return errors.New("content.ref is required for type: git")
		}
		if s.Host != "" && s.Host != "github" && s.Host != "gitlab" {
			return fmt.Errorf("content.host must be github or gitlab, got %q", s.Host)
		}
	case TypeSubmodule:
		if s.Submodule == "" {
			return errors.New("content.submodule (path) is required for type: submodule")
		}
	case TypeURL:
		if s.URL == "" {
			return errors.New("content.url is required for type: url")
		}
		u, perr := url.Parse(s.URL)
		if perr != nil || !strings.EqualFold(u.Scheme, "https") {
			return fmt.Errorf("content.url must be an https:// URL, got %q", s.URL)
		}
		// SECURITY: sha256 is mandatory, not merely recommended. It's what
		// makes a fetched archive verifiable at all — FetchURL refuses to
		// extract or cache anything that doesn't match — and what the
		// on-disk cache is keyed on (content is immutable by digest, so a
		// restart with the same digest never re-fetches). A type: url
		// source with no digest would be an unauthenticated fetch of
		// arbitrary remote content into the process that serves it.
		if s.SHA256 == "" {
			return errors.New("content.sha256 is required for type: url (the archive's digest — required so fetched content is verifiable, and used to key the local cache)")
		}
		if !isHex64(s.SHA256) {
			return fmt.Errorf("content.sha256 must be 64 hex characters (a sha256 digest), got %q (%d chars)", s.SHA256, len(s.SHA256))
		}
	default:
		return fmt.Errorf("content.type must be one of none|local|git|submodule|url, got %q", s.Type)
	}
	if s.Layout.Wiki == "" {
		return fmt.Errorf("content.layout.wiki is required for type: %s", s.Type)
	}
	return nil
}

// isHex64 reports whether s is exactly 64 hexadecimal characters — the
// shape of a hex-encoded sha256 digest. Case-insensitive: content.sha256
// may be written in either case in content-source.yaml.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		isHexDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHexDigit {
			return false
		}
	}
	return true
}

// MovingRef reports whether ref looks like a non-pinned (moving) reference —
// i.e. not a tag-like or full SHA. Used to warn that the build is not
// reproducible. Heuristic, not authoritative.
func MovingRef(ref string) bool {
	if ref == "" {
		return true
	}
	// A 40- or 7-to-40-char hex string is a commit SHA (pinned).
	isHex := len(ref) >= 7
	for _, c := range ref {
		isHexDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHexDigit {
			isHex = false
			break
		}
	}
	if isHex {
		return false
	}
	// Tag-like (contains a dot or starts with v + digit) -> treat as pinned.
	if len(ref) > 1 && ref[0] == 'v' && ref[1] >= '0' && ref[1] <= '9' {
		return false
	}
	for _, c := range ref {
		if c == '.' {
			return false
		}
	}
	return true
}
