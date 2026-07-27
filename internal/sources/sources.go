// Package sources embeds the ingestion source registry
// (sources.yaml), the per-source-type sub-agent prompts, and the
// page templates from the configured content source. They're embedded
// into the binary at build time so `mk ingest` works without the
// content repo on disk.
//
// At run time the embed can be overridden by a directory on disk (see
// UseFS and internal/kbdir) — the --kb-dir flag / MEERKAT_KB_DIR env
// var let an operator update ingestion config without rebuilding the
// binary. The embedded content remains the fallback when no directory
// is configured.
package sources

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

//go:embed all:etc
var etcFS embed.FS

// currentFS is the filesystem All/Prompt/Template/FS actually read
// from. It defaults to the build-time embedded registry and can be
// redirected to a directory on disk via UseFS. See kb.UseFS's doc
// comment — the same global-state caveats, and the same reason this is
// an atomic.Pointer rather than a bare fs.FS var, apply here: fs.FS is
// a 2-word interface value, so an unsynchronized write racing a read is
// a torn read on those words, independent of how disciplined today's
// single call-order (cobra's PersistentPreRunE, before any reader
// starts) happens to be.
var currentFS atomic.Pointer[fs.FS]

func init() {
	setFS(etcFS)
}

// setFS atomically stores fsys as the active filesystem.
func setFS(fsys fs.FS) {
	currentFS.Store(&fsys)
}

// loadFS atomically loads the active filesystem. Every internal read
// path (All, Prompt, Template) goes through this rather than touching
// currentFS directly, so a concurrent UseFS call can never produce a
// torn read of the interface value.
func loadFS() fs.FS { return *currentFS.Load() }

// UseFS redirects All/Prompt/Template/FS to read from fsys instead of
// the embedded registry. Passing nil restores the embedded default.
func UseFS(fsys fs.FS) {
	if fsys == nil {
		setFS(etcFS)
		return
	}
	setFS(fsys)
}

// Source is one entry in sources.yaml — see the comment block at the
// top of that file for field semantics.
type Source struct {
	ID string `yaml:"id"               json:"id"`
	// Host is the git hosting provider: github | gitlab | local.
	// Empty is inferred by Normalize: "local" for synthesised/pdfs
	// sources, "github" otherwise.
	Host string `yaml:"host,omitempty"   json:"host,omitempty"`
	// Type is the enumeration kind: files | repos-in-group | pdfs |
	// synthesised (see KindFiles etc.). The hosting provider lives in Host.
	// Legacy host-flavored values (gitlab, gitlab-group, pdf-corpus) are
	// accepted and rewritten by Normalize.
	Type           string    `yaml:"type"             json:"type"`
	Repo           string    `yaml:"repo,omitempty"   json:"repo,omitempty"`
	Group          string    `yaml:"group,omitempty"  json:"group,omitempty"`
	Path           string    `yaml:"path,omitempty"   json:"path,omitempty"` // for pdf-corpus
	TargetCategory string    `yaml:"target_category"  json:"target_category"`
	Enumerate      Enumerate `yaml:"enumerate"        json:"enumerate"`
	Template       string    `yaml:"template"         json:"template"` // basename under templates/
	Prompt         string    `yaml:"prompt"           json:"prompt"`   // path under ingestion/, e.g. "prompts/policy.md"
	Schedule       string    `yaml:"schedule"         json:"schedule"` // weekly | monthly | on-change
	Enrichment     []string  `yaml:"enrichment,omitempty" json:"enrichment,omitempty"`
	SeedList       string    `yaml:"seed_list,omitempty"  json:"seed_list,omitempty"`
	Model          string    `yaml:"model,omitempty"  json:"model,omitempty"` // optional per-source model override
}

// Enumerate describes how to discover items inside a Source.
type Enumerate struct {
	Kind             string `yaml:"kind"                       json:"kind"` // files | repos-in-group | pdfs
	Glob             string `yaml:"glob,omitempty"             json:"glob,omitempty"`
	Path             string `yaml:"path,omitempty"             json:"path,omitempty"` // restrict to subdir of repo
	IncludeSubgroups bool   `yaml:"include_subgroups,omitempty" json:"include_subgroups,omitempty"`
}

type yamlRoot struct {
	Sources []Source `yaml:"sources"`
}

// ErrNotFound is returned when a named source is not registered.
var ErrNotFound = errors.New("source not found")

// All returns every source registered in sources.yaml, in
// declaration order. Sources commented out in the YAML are not
// returned.
func All() ([]Source, error) {
	body, err := fs.ReadFile(loadFS(), "etc/sources.yaml")
	if errors.Is(err, fs.ErrNotExist) {
		// No source registry embedded (e.g. a build with no configured
		// content source). Treat as an empty registry rather than an error.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sources.yaml: %w", err)
	}
	var root yamlRoot
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("parse sources.yaml: %w", err)
	}
	for i := range root.Sources {
		root.Sources[i].Normalize()
	}
	return root.Sources, nil
}

// Enumeration kinds (the post-normalization values of Source.Type).
const (
	KindFiles        = "files"          // enumerate markdown files in a repo
	KindReposInGroup = "repos-in-group" // enumerate repos in a group/org
	KindPDFs         = "pdfs"           // a corpus of PDFs
	KindSynthesised  = "synthesised"    // generated, not fetched from a host
)

// Normalize maps legacy, host-flavored type values onto the host-agnostic
// model (Host = where, Type = enumeration kind) and infers a sensible Host
// when one is not given. Idempotent.
func (s *Source) Normalize() {
	switch s.Type {
	case "gitlab":
		s.setHostIfEmpty("gitlab")
		s.Type = KindFiles
	case "gitlab-group":
		s.setHostIfEmpty("gitlab")
		s.Type = KindReposInGroup
	case "github":
		s.setHostIfEmpty("github")
		s.Type = KindFiles
	case "pdf-corpus":
		s.setHostIfEmpty("local")
		s.Type = KindPDFs
	}
	if s.Host == "" {
		switch s.Type {
		case KindSynthesised, KindPDFs:
			s.Host = "local"
		default:
			s.Host = "github"
		}
	}
}

func (s *Source) setHostIfEmpty(h string) {
	if s.Host == "" {
		s.Host = h
	}
}

// Get returns the source with the given id.
func Get(id string) (Source, error) {
	all, err := All()
	if err != nil {
		return Source{}, err
	}
	for _, s := range all {
		if s.ID == id {
			return s, nil
		}
	}
	return Source{}, ErrNotFound
}

// Prompt returns the contents of an embedded sub-agent prompt.
// promptPath is the value of Source.Prompt, e.g. "prompts/policy.md".
func Prompt(promptPath string) (string, error) {
	// Tolerate either "prompts/policy.md" or just "policy.md".
	rel := strings.TrimPrefix(promptPath, "prompts/")
	body, err := fs.ReadFile(loadFS(), path.Join("etc", "prompts", rel))
	if err != nil {
		return "", fmt.Errorf("read prompt %q: %w", promptPath, err)
	}
	return string(body), nil
}

// Template returns the contents of an embedded page template.
// templateName is the value of Source.Template, e.g. "policy.md".
func Template(templateName string) (string, error) {
	body, err := fs.ReadFile(loadFS(), path.Join("etc", "templates", templateName))
	if err != nil {
		return "", fmt.Errorf("read template %q: %w", templateName, err)
	}
	return string(body), nil
}

// FS returns the filesystem All/Prompt/Template currently read from —
// the embedded registry by default, or a disk directory after UseFS.
func FS() fs.FS { return loadFS() }
