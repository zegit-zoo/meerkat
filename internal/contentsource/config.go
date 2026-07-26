// Package contentsource resolves meerkat's build-time knowledge-base
// content source from a declarative content-source.yaml and populates the
// package-local embed directories (internal/kb/content, internal/sources/etc)
// that go:embed bakes into the binary.
//
// It is a build-time tool (invoked by `make sync` / the GoReleaser
// before-hook), not part of the shipped runtime: the engine still reads only
// its embedded FS at run time.
package contentsource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
)

// Config is the top-level content-source.yaml document.
type Config struct {
	Content Source `yaml:"content"`
}

// Source declares a single build-time content source.
type Source struct {
	Type string `yaml:"type"` // none | local | git | submodule

	// type: local
	Path string `yaml:"path,omitempty"` // content root, relative to repo root or absolute

	// type: git
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

	// type: submodule
	Submodule string `yaml:"submodule,omitempty"` // submodule path (e.g. "kb")

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
func Load(repoRoot string) (Config, error) {
	body, err := os.ReadFile(filepath.Join(repoRoot, ConfigFile)) //nolint:gosec // G304: build-time tool reading its own config from the repo root.
	if errors.Is(err, os.ErrNotExist) {
		return Config{Content: Source{Type: TypeNone}}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", ConfigFile, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", ConfigFile, err)
	}
	if cfg.Content.Type == "" {
		cfg.Content.Type = TypeNone
	}
	cfg.Content.Layout = mergeLayout(cfg.Content.Layout)
	if err := cfg.Content.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func mergeLayout(l Layout) Layout {
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
	default:
		return fmt.Errorf("content.type must be one of none|local|git|submodule, got %q", s.Type)
	}
	if s.Layout.Wiki == "" {
		return fmt.Errorf("content.layout.wiki is required for type: %s", s.Type)
	}
	return nil
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
