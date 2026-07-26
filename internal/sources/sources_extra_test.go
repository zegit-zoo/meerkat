package sources

import (
	"io/fs"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests exercise the registry helpers' non-embedded code paths
// (error handling, FS access, full-field parsing) so they hold regardless
// of whether any sources.yaml is embedded at build time.

func TestPrompt_MissingReturnsError(t *testing.T) {
	if _, err := Prompt("prompts/does-not-exist.md"); err == nil {
		t.Error("expected error for a missing prompt")
	}
}

func TestTemplate_MissingReturnsError(t *testing.T) {
	if _, err := Template("does-not-exist.md"); err == nil {
		t.Error("expected error for a missing template")
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ inType, inHost, wantType, wantHost string }{
		{"gitlab", "", "files", "gitlab"},
		{"gitlab-group", "", "repos-in-group", "gitlab"},
		{"pdf-corpus", "", "pdfs", "local"},
		{"github", "", "files", "github"},
		{"files", "", "files", "github"}, // host inferred
		{"synthesised", "", "synthesised", "local"},
		{"files", "gitlab", "files", "gitlab"}, // explicit host preserved
		{"repos-in-group", "github", "repos-in-group", "github"},
	}
	for _, c := range cases {
		s := Source{Type: c.inType, Host: c.inHost}
		s.Normalize()
		if s.Type != c.wantType || s.Host != c.wantHost {
			t.Errorf("Normalize(type=%q,host=%q) = (%q,%q), want (%q,%q)",
				c.inType, c.inHost, s.Type, s.Host, c.wantType, c.wantHost)
		}
	}
}

func TestFS_NonNil(t *testing.T) {
	if FS() == nil {
		t.Fatal("FS() returned nil")
	}
	// The embedded tree always contains at least the etc/ root.
	if _, err := fs.Stat(FS(), "etc"); err != nil {
		t.Errorf("expected embedded etc/ dir: %v", err)
	}
}

func TestSource_ParsesAllFields(t *testing.T) {
	snippet := `sources:
  - id: backend-systems
    host: gitlab
    type: gitlab-group
    group: your-org/backend
    path: services/
    target_category: systems
    enumerate:
      kind: repos-in-group
      glob: "*.md"
      include_subgroups: true
    template: service.md
    prompt: prompts/service.md
    schedule: weekly
    enrichment: [past-incidents, service-catalog]
    model: openai/gpt-5.5-fast
  - id: concepts
    type: synthesised
    target_category: concepts
    seed_list: seeds/concepts.txt
    enumerate:
      kind: files
`
	var root yamlRoot
	if err := yaml.Unmarshal([]byte(snippet), &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(root.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(root.Sources))
	}
	s := root.Sources[0]
	if s.Host != "gitlab" || s.Type != "gitlab-group" || s.Group != "your-org/backend" {
		t.Errorf("unexpected source[0]: %+v", s)
	}
	if !s.Enumerate.IncludeSubgroups || s.Enumerate.Kind != "repos-in-group" {
		t.Errorf("enumerate not parsed: %+v", s.Enumerate)
	}
	if len(s.Enrichment) != 2 || s.Enrichment[0] != "past-incidents" {
		t.Errorf("enrichment not parsed: %v", s.Enrichment)
	}
	if s.Model != "openai/gpt-5.5-fast" {
		t.Errorf("model = %q", s.Model)
	}
	if root.Sources[1].SeedList != "seeds/concepts.txt" {
		t.Errorf("seed_list = %q", root.Sources[1].SeedList)
	}
}
