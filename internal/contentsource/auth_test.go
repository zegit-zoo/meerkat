package contentsource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

const validAuthBlock = `
auth:
  resource: https://mcp.example.com/mcp
  providers:
    - issuer: https://login.example.com/tenant/v2.0
      audience: api://meerkat
      claims:
        groups: roles
        email: preferred_username
  rules:
    - name: sre
      groups: [sre]
      collections: [runbooks]
      capabilities: [read, team-write]
    - name: admins
      groups: [platform-admins]
      collections: ["*"]
      capabilities: [admin]
`

func TestParseConfig_AuthBlockAlongsideContent(t *testing.T) {
	cfg, err := parseConfig([]byte("content:\n  type: local\n  path: ./kb\n"+validAuthBlock), "test.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Auth == nil {
		t.Fatal("auth: block was not parsed")
	}
	if cfg.Auth.Resource != "https://mcp.example.com/mcp" {
		t.Errorf("resource = %q", cfg.Auth.Resource)
	}
	if len(cfg.Auth.Providers) != 1 {
		t.Fatalf("providers = %d", len(cfg.Auth.Providers))
	}
	if got := cfg.Auth.Providers[0].Claims.Groups; got != "roles" {
		t.Errorf("claims.groups = %q, want the explicit mapping", got)
	}
	if len(cfg.Auth.Rules) != 2 {
		t.Fatalf("rules = %d", len(cfg.Auth.Rules))
	}
	// The content block is untouched by the presence of auth:.
	if cfg.Content.Type != TypeLocal || cfg.Content.Path != "./kb" {
		t.Errorf("content = %+v", cfg.Content)
	}
}

func TestParseConfig_AuthBlockAlongsideCollections(t *testing.T) {
	body := `
collections:
  - name: runbooks
    type: local
    path: ./runbooks
  - name: architecture
    type: local
    path: ./arch
` + validAuthBlock
	cfg, err := parseConfig([]byte(body), "test.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Collections) != 2 {
		t.Fatalf("collections = %d", len(cfg.Collections))
	}
	if cfg.Auth == nil || len(cfg.Auth.Rules) != 2 {
		t.Fatal("auth: block was not parsed alongside collections:")
	}
}

func TestParseConfig_NoAuthBlockIsNil(t *testing.T) {
	cfg, err := parseConfig([]byte("content:\n  type: local\n  path: ./kb\n"), "test.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Auth != nil {
		t.Fatal("a config with no auth: key must leave Auth nil — that is the back-compat state")
	}
	if cfg.Auth.Enabled() {
		t.Fatal("a nil auth config is not enabled")
	}
}

func TestParseConfig_InvalidAuthFailsTheLoad(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "unknown capability",
			body: `
content: {type: local, path: ./kb}
auth:
  resource: https://mcp.example.com/mcp
  providers: [{issuer: "https://i.example.com", audience: a}]
  rules:
    - name: oops
      collections: [x]
      capabilities: [write]
`,
			wantErr: `unknown capability "write"`,
		},
		{
			name: "rules without providers",
			body: `
content: {type: local, path: ./kb}
auth:
  rules:
    - collections: [x]
`,
			wantErr: "no auth.providers are configured",
		},
		{
			name: "missing resource",
			body: `
content: {type: local, path: ./kb}
auth:
  providers: [{issuer: "https://i.example.com"}]
`,
			wantErr: "auth.resource is required",
		},
		{
			name: "http issuer",
			body: `
content: {type: local, path: ./kb}
auth:
  resource: https://mcp.example.com/mcp
  providers: [{issuer: "http://i.example.com", audience: a}]
`,
			wantErr: "must use https",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.body), "test.yaml")
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "test.yaml") {
				t.Errorf("the error should name the file: %v", err)
			}
		})
	}
}

func TestLoadAuthFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.yaml")
	if err := os.WriteFile(path, []byte(validAuthBlock), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadAuthFile(path)
	if err != nil {
		t.Fatalf("LoadAuthFile: %v", err)
	}
	if !cfg.Enabled() {
		t.Error("a config with providers should be enabled")
	}
	pol, err := authz.NewPolicy(cfg)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	g := pol.Evaluate(authz.Identity{Subject: "u", Groups: []string{"sre"}})
	if !g.CanRead("runbooks") || g.CanRead("secrets") {
		t.Errorf("policy did not round-trip through the standalone file: %v", g.Named())
	}
}

func TestLoadAuthFile_Errors(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadAuthFile(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Error("a missing --auth-config file must be an error: the operator named it")
	}

	noBlock := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(noBlock, []byte("content:\n  type: local\n  path: ./kb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAuthFile(noBlock)
	if err == nil || !strings.Contains(err.Error(), "no auth: block") {
		t.Errorf("err = %v, want it to say there is no auth: block", err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("auth:\n  providers: [{issuer: \"https://i.example.com\"}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuthFile(bad); err == nil || !strings.Contains(err.Error(), "auth.resource is required") {
		t.Errorf("err = %v, want validation to run on a standalone file too", err)
	}
}

func TestLoadRuntimeAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFile)
	if err := os.WriteFile(path, []byte("content:\n  type: local\n  path: ./kb\n"+validAuthBlock), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRuntimeAuth(path)
	if err != nil {
		t.Fatalf("LoadRuntimeAuth: %v", err)
	}
	if cfg == nil || len(cfg.Rules) != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadRuntimeAuth_NoConfigIsNotAnError(t *testing.T) {
	// An isolated working directory with no content-source.yaml and no
	// user config dir entry: discovery finds nothing, which is the
	// overwhelmingly common state and must not be an error.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	cfg, err := LoadRuntimeAuth("")
	if err != nil {
		t.Fatalf("LoadRuntimeAuth: %v", err)
	}
	if cfg != nil {
		t.Fatalf("cfg = %+v, want nil", cfg)
	}
}

func TestLoadRuntimeAuth_ConfigWithoutAuthIsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFile)
	if err := os.WriteFile(path, []byte("content:\n  type: local\n  path: ./kb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRuntimeAuth(path)
	if err != nil {
		t.Fatalf("LoadRuntimeAuth: %v", err)
	}
	if cfg != nil {
		t.Fatalf("cfg = %+v, want nil for a config with no auth: block", cfg)
	}
}
