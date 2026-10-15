package sources

import (
	"strings"
	"testing"
)

// prompt: and template: names stay inside their directory: path.Join
// would otherwise clean ".." away and read another registry file
// (meerkat-mob#35).
func TestPromptAndTemplate_RefuseNamesOutsideTheirDirectory(t *testing.T) {
	for _, name := range []string{"../sources.yaml", "prompts/../../sources.yaml", "a/../../x.md", "./x.md", "/etc/passwd", `..\x.md`, ""} {
		if _, err := Prompt(name); err == nil || !strings.Contains(err.Error(), "read prompt") {
			t.Errorf("Prompt(%q) = %v; want a refusal", name, err)
		}
		if _, err := Template(name); err == nil || !strings.Contains(err.Error(), "read template") {
			t.Errorf("Template(%q) = %v; want a refusal", name, err)
		}
	}
	if err := checkRegistryName("policy.md"); err != nil {
		t.Errorf("plain name refused: %v", err)
	}
	if err := checkRegistryName("sub/policy.md"); err != nil {
		t.Errorf("nested name refused: %v", err)
	}
}
