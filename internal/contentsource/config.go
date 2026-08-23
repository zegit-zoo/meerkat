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

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// ConfigFile is the repo-root config filename.
const ConfigFile = "content-source.yaml"

// DefaultCollectionName is the name given to the collection a
// single-source (`content:`) config resolves to, and to the embedded
// fallback. Nothing in a legacy config mentions collections at all, so
// the name is synthesised here rather than read from the file — it's
// what `--collection`/`mk list --collections` show, and what the
// multi-collection routing rules treat as "the one collection".
const DefaultCollectionName = "default"

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
	// TypeGCS loads content from a Google Cloud Storage bucket, either as
	// a single .tar.gz object or as an object prefix treated as a
	// directory tree. Credentials resolve through Application Default
	// Credentials / Workload Identity Federation — the schema carries no
	// key material of any kind. Runtime-only, like TypeURL. See FetchGCS.
	TypeGCS = "gcs"
)

// Config is the top-level content-source.yaml document.
//
// Exactly one of Content and Collections carries the configuration:
//
//   - `content:` — the original single-source form. Still the whole
//     schema for the overwhelmingly common one-KB deployment, and
//     unchanged in behaviour: it resolves to one collection named
//     DefaultCollectionName.
//   - `collections:` — an ordered list of named sources, each reusing
//     the same Source schema `content:` uses (type/path/url/bucket/...
//     plus a per-collection layout). Order is significant: it is the
//     order collections are searched, listed, and disambiguated in (see
//     internal/collections).
//
// Setting both is a configuration error rather than a merge: a reader
// of the file should never have to work out which of two content
// declarations won.
//
// The optional `auth:` block is orthogonal to both: it declares the
// OIDC providers whose tokens the hosted MCP server accepts and the
// rules mapping a verified caller to collections and capabilities. It
// is read only by `mk mcp serve-http`; every other surface ignores it,
// so adding one cannot change what the CLI, stdio MCP or the
// static-token HTTP server do. See internal/authz.
type Config struct {
	Content     Source        `yaml:"content"`
	Collections []Collection  `yaml:"collections,omitempty"`
	Auth        *authz.Config `yaml:"auth,omitempty"`
}

// Collection is one named entry of a `collections:` list — a Source
// (inlined, so every existing source key is written exactly as it is
// under `content:`) plus the name it is mounted under.
type Collection struct {
	// Name is how the collection is addressed: `--collection <name>`,
	// the MCP tools' `collection` argument, and the `<name>:<page-id>`
	// qualified page ID form. Constrained to [A-Za-z0-9][A-Za-z0-9_-]*
	// so a name is always safe to use unquoted in a CLI argument, as a
	// qualified-ID prefix (hence: no colon), and — with the per-
	// collection authorization work this is the foundation for — as a
	// stable identifier in a policy document.
	Name   string `yaml:"name"`
	Source `yaml:",inline"`
}

// maxCollectionNameLen bounds a collection name. Nothing technical
// requires a bound; it exists so a pathological name can't turn every
// error message and tool description into a wall of text.
const maxCollectionNameLen = 64

// validCollectionName reports whether name matches the documented
// [A-Za-z0-9][A-Za-z0-9_-]* shape (see Collection.Name).
func validCollectionName(name string) bool {
	if name == "" || len(name) > maxCollectionNameLen {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
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

	// type: gcs — runtime-capable (see FetchGCS). Bucket is required;
	// exactly one of Object (a .tar.gz bundle) or Prefix (an object
	// prefix served as a directory tree) selects the mode.
	//
	// There is deliberately no credentials/key-file field: authentication
	// is Application Default Credentials / Workload Identity Federation,
	// resolved by the Google client library from the ambient environment.
	// A static service-account key has nowhere to be named here, which is
	// the point.
	Bucket string `yaml:"bucket,omitempty"`
	Object string `yaml:"object,omitempty"`
	Prefix string `yaml:"prefix,omitempty"`
	// Generation optionally pins Object to one exact object generation.
	// When set, no metadata lookup is made at all and the pinned
	// generation is fetched directly — the reproducible-deployment
	// equivalent of pinning a type: url source by digest. When unset, the
	// object's current generation is resolved once at startup and used
	// both for the conditional read and as the cache key. Prefix mode
	// ignores it (a prefix spans many objects, each with its own
	// generation).
	Generation int64 `yaml:"generation,omitempty"`

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
	// Validate auth: at load time so a policy mistake fails the process
	// rather than the first request that would have been affected by it
	// — an unknown capability or a rule with no collections silently
	// granting nothing is exactly the class of error that only shows up
	// as "why can't this user see anything" weeks later.
	if err := cfg.Auth.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", displayPath, err)
	}
	if len(cfg.Collections) > 0 {
		// Mutually exclusive, not merged — see Config's doc comment. Only
		// a content: block that actually declares something conflicts; the
		// TypeNone default filled in above (i.e. no content: key at all)
		// is what a collections-only file legitimately looks like.
		if cfg.Content.Type != TypeNone {
			return Config{}, fmt.Errorf("%s: content: and collections: are mutually exclusive — declare the single source under content:, or move it into collections: as a named entry", displayPath)
		}
		if err := validateCollections(cfg.Collections); err != nil {
			return Config{}, err
		}
		for i := range cfg.Collections {
			cfg.Collections[i].Layout = MergeLayout(cfg.Collections[i].Layout)
		}
		// Re-validate now that layouts are defaulted (validateCollections
		// checked names + type-specific fields; layout.wiki is only
		// meaningful after the merge).
		for _, c := range cfg.Collections {
			if err := c.validate(fmt.Sprintf("collections[%s]", c.Name)); err != nil {
				return Config{}, err
			}
		}
		return cfg, nil
	}
	if err := cfg.Content.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateCollections checks the collection-level invariants that
// Source.validate can't see: names are present, well-formed and unique,
// and no entry is type: none (an unserveable collection is a config
// mistake, not an empty-KB fallback — that fallback only exists for the
// single-source form).
func validateCollections(cols []Collection) error {
	seen := make(map[string]bool, len(cols))
	for i, c := range cols {
		if !validCollectionName(c.Name) {
			return fmt.Errorf("collections[%d].name %q is not a valid collection name: use letters/digits, optionally with - or _ after the first character (max %d chars)", i, c.Name, maxCollectionNameLen)
		}
		if seen[c.Name] {
			return fmt.Errorf("collections: duplicate collection name %q — names address a collection (--collection, <name>:<page-id>) and must be unique", c.Name)
		}
		seen[c.Name] = true
		if c.Type == "" || c.Type == TypeNone {
			return fmt.Errorf("collections[%s].type is required (none is not meaningful for a named collection)", c.Name)
		}
	}
	return nil
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

// Validate checks the source for the required fields of its type. It is
// the `content:`-rooted form of validate — errors name the offending
// key as "content.<field>". A named collection validates the same
// fields under a "collections[<name>]." prefix instead.
func (s Source) Validate() error { return s.validate("content") }

// validate is Validate with the field path a message should name made
// explicit (see Validate). p is the parent path — "content" or
// "collections[docs]" — with no trailing dot.
func (s Source) validate(p string) error {
	switch s.Type {
	case TypeNone:
		return nil
	case TypeLocal:
		if s.Path == "" {
			return fmt.Errorf("%s.path is required for type: local", p)
		}
	case TypeGit:
		if s.Repo == "" {
			return fmt.Errorf("%s.repo is required for type: git", p)
		}
		if s.Ref == "" {
			return fmt.Errorf("%s.ref is required for type: git", p)
		}
		if s.Host != "" && s.Host != "github" && s.Host != "gitlab" {
			return fmt.Errorf("%s.host must be github or gitlab, got %q", p, s.Host)
		}
	case TypeSubmodule:
		if s.Submodule == "" {
			return fmt.Errorf("%s.submodule (path) is required for type: submodule", p)
		}
	case TypeGCS:
		if err := s.validateGCS(p); err != nil {
			return err
		}
	case TypeURL:
		if s.URL == "" {
			return fmt.Errorf("%s.url is required for type: url", p)
		}
		u, perr := url.Parse(s.URL)
		if perr != nil || !strings.EqualFold(u.Scheme, "https") {
			return fmt.Errorf("%s.url must be an https:// URL, got %q", p, s.URL)
		}
		// SECURITY: sha256 is mandatory, not merely recommended. It's what
		// makes a fetched archive verifiable at all — FetchURL refuses to
		// extract or cache anything that doesn't match — and what the
		// on-disk cache is keyed on (content is immutable by digest, so a
		// restart with the same digest never re-fetches). A type: url
		// source with no digest would be an unauthenticated fetch of
		// arbitrary remote content into the process that serves it.
		if s.SHA256 == "" {
			return fmt.Errorf("%s.sha256 is required for type: url (the archive's digest — required so fetched content is verifiable, and used to key the local cache)", p)
		}
		if !isHex64(s.SHA256) {
			return fmt.Errorf("%s.sha256 must be 64 hex characters (a sha256 digest), got %q (%d chars)", p, s.SHA256, len(s.SHA256))
		}
	default:
		return fmt.Errorf("%s.type must be one of none|local|git|submodule|url|gcs, got %q", p, s.Type)
	}
	if s.Layout.Wiki == "" {
		return fmt.Errorf("%s.layout.wiki is required for type: %s", p, s.Type)
	}
	return nil
}

// validateGCS checks the type: gcs fields. Exactly one of object (a
// .tar.gz bundle) or prefix (a directory tree) must be set: they are
// two different retrieval modes with two different cache keys, and
// guessing between them from an ambiguous config would make it
// impossible to tell from the file which one a deployment gets.
func (s Source) validateGCS(p string) error {
	if s.Bucket == "" {
		return fmt.Errorf("%s.bucket is required for type: gcs", p)
	}
	if strings.Contains(s.Bucket, "/") {
		return fmt.Errorf("%s.bucket must be a bucket name, not a path or gs:// URL, got %q", p, s.Bucket)
	}
	switch {
	case s.Object == "" && s.Prefix == "":
		return fmt.Errorf("%s: type: gcs needs either object: (a .tar.gz bundle) or prefix: (an object prefix served as a directory tree)", p)
	case s.Object != "" && s.Prefix != "":
		return fmt.Errorf("%s: type: gcs takes object: or prefix:, not both — object: fetches one .tar.gz bundle, prefix: serves an object prefix as a directory tree", p)
	}
	if s.Object != "" && s.Generation < 0 {
		return fmt.Errorf("%s.generation must be a positive object generation, got %d", p, s.Generation)
	}
	if s.SHA256 != "" && !isHex64(s.SHA256) {
		return fmt.Errorf("%s.sha256 must be 64 hex characters (a sha256 digest), got %q (%d chars)", p, s.SHA256, len(s.SHA256))
	}
	if s.Prefix != "" && s.SHA256 != "" {
		return fmt.Errorf("%s.sha256 applies to object: (one archive's bytes), not to prefix: (many objects)", p)
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
