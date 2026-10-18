package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"
)

// knowledgeBaseSpecSchema loads spec's schema from the generated CRD, so
// the test checks what the API server enforces, not the Go markers.
func knowledgeBaseSpecSchema(t *testing.T) *spec.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "operator", "crds", "meerkat-mob.dev_knowledgebases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `json:"name"`
				Schema struct {
					OpenAPIV3Schema struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	for _, v := range crd.Spec.Versions {
		if v.Name != GroupVersion.Version {
			continue
		}
		var s spec.Schema
		if err := json.Unmarshal(v.Schema.OpenAPIV3Schema.Properties["spec"], &s); err != nil {
			t.Fatal(err)
		}
		return &s
	}
	t.Fatalf("no %s version in the KnowledgeBase CRD", GroupVersion.Version)
	return nil
}

func TestKnowledgeBaseCRD_ParentPattern(t *testing.T) {
	validator := validate.NewSchemaValidator(knowledgeBaseSpecSchema(t), nil, "", strfmt.Default)
	parent := func(p any) map[string]any {
		s := map[string]any{"source": map[string]any{"s3": map[string]any{"bucket": "kb"}}}
		if p != nil {
			s["parent"] = p
		}
		return s
	}
	for _, tc := range []struct {
		name   string
		parent any
		ok     bool
	}{
		{"absent", nil, true},
		// Blanking the value in kubectl edit sends "" (meerkat#202).
		{"empty", "", true},
		{"name", "root", true},
		{"subdomain", "team.platform-kb", true},
		{"space and capitals", "Foo Bar", false},
		{"capitals", "Root", false},
		{"leading dash", "-root", false},
		{"trailing dot", "root.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := validator.Validate(parent(tc.parent))
			if res.IsValid() != tc.ok {
				t.Errorf("parent %q: valid = %v, want %v (errors: %v)", tc.parent, res.IsValid(), tc.ok, res.Errors)
			}
		})
	}
}
