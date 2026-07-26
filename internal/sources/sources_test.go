package sources

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAll: at least the seed-known sources are registered, in
// declaration order.
// skipIfNoSources skips a registry-dependent test when no sources are
// embedded (e.g. a build with the content source stripped). See
// a build with no embedded KB content.
func skipIfNoSources(t *testing.T) {
	t.Helper()
	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) == 0 {
		t.Skip("no sources embedded")
	}
}

func TestAll(t *testing.T) {
	skipIfNoSources(t)
	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	wantIDs := []string{
		"adr", "threat-models", "security-decisions", "requirements",
		"dev-portal", "pipelines", "base-images", "runbooks",
		"backend-systems", "frontend-systems", "policies", "concepts",
	}
	have := map[string]bool{}
	for _, s := range all {
		have[s.ID] = true
		if s.Type == "" {
			t.Errorf("source %s missing type", s.ID)
		}
		if s.TargetCategory == "" {
			t.Errorf("source %s missing target_category", s.ID)
		}
	}
	for _, id := range wantIDs {
		if !have[id] {
			t.Errorf("expected source %q to be registered", id)
		}
	}
}

// TestGet returns the registered source.
func TestGet(t *testing.T) {
	skipIfNoSources(t)
	s, err := Get("policies")
	if err != nil {
		t.Fatalf("Get(policies): %v", err)
	}
	if s.Type != "pdf-corpus" {
		t.Errorf("policies.type = %q, want pdf-corpus", s.Type)
	}
}

// TestGet_NotFound surfaces ErrNotFound for unknown ids.
func TestGet_NotFound(t *testing.T) {
	_, err := Get("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestPrompt: every source's declared prompt resolves to a
// non-empty body. Catches typos / missing files at test time.
func TestPrompt_AllSourcesResolve(t *testing.T) {
	all, _ := All()
	for _, s := range all {
		t.Run(s.ID, func(t *testing.T) {
			body, err := Prompt(s.Prompt)
			if err != nil {
				t.Fatalf("Prompt(%s): %v", s.Prompt, err)
			}
			if !strings.Contains(body, "Instructions") &&
				!strings.Contains(body, "instructions") {
				t.Errorf("prompt %s lacks an Instructions section", s.Prompt)
			}
		})
	}
}

// TestTemplate: every source's declared template resolves and
// includes a frontmatter delimiter.
func TestTemplate_AllSourcesResolve(t *testing.T) {
	all, _ := All()
	for _, s := range all {
		t.Run(s.ID, func(t *testing.T) {
			body, err := Template(s.Template)
			if err != nil {
				t.Fatalf("Template(%s): %v", s.Template, err)
			}
			if !strings.Contains(body, "---") {
				t.Errorf("template %s missing frontmatter delimiters", s.Template)
			}
		})
	}
}

// TestSource_HostField verifies that a YAML snippet with host: github
// parses into Source.Host correctly. This test is independent of any
// embedded sources.yaml.
func TestSource_HostField(t *testing.T) {
	snippet := `sources:
  - id: my-gh-repo
    host: github
    type: github
    repo: org/my-repo
    target_category: concepts
    enumerate:
      kind: files
      glob: "**/*.md"
    template: default.md
    prompt: prompts/general.md
    schedule: weekly
`
	var root yamlRoot
	if err := yaml.Unmarshal([]byte(snippet), &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(root.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(root.Sources))
	}
	s := root.Sources[0]
	if s.Host != "github" {
		t.Errorf("Host = %q, want %q", s.Host, "github")
	}
	if s.ID != "my-gh-repo" {
		t.Errorf("ID = %q", s.ID)
	}
}

// TestPrompt_TolerateBareName: callers may pass either
// "prompts/policy.md" or just "policy.md".
func TestPrompt_TolerateBareName(t *testing.T) {
	skipIfNoSources(t)
	a, err := Prompt("prompts/policy.md")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Prompt("policy.md")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("Prompt should accept both prefixed and bare names")
	}
}
