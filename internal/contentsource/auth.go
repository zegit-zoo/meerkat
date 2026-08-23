package contentsource

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// auth.go resolves the `auth:` block — the OIDC providers and
// collection access rules the hosted MCP server enforces. See
// internal/authz for the schema and docs/design/hosted-mcp.md for the
// model.

// authDocument is a file that carries only an auth: block. It is the
// same key, at the same nesting, as in a content-source.yaml, so the
// two forms are copy-pasteable between each other.
type authDocument struct {
	Auth *authz.Config `yaml:"auth"`
}

// LoadAuthFile reads a standalone auth policy file: a YAML document
// with a top-level `auth:` key.
//
// It exists because content and access policy have different lifecycles
// and often different owners. A content-source.yaml is frequently baked
// into an image or shared across environments, while the policy — which
// tenant, which groups, which collections — differs per environment and
// changes when a team does. Keeping them in one file is supported (see
// LoadRuntimeAuth) and keeping them apart is supported; neither is
// privileged.
func LoadAuthFile(path string) (*authz.Config, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: path is an operator-supplied config location (--auth-config), not attacker-influenced input.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc authDocument
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Auth == nil {
		return nil, fmt.Errorf("%s has no auth: block", path)
	}
	if err := doc.Auth.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc.Auth, nil
}

// LoadRuntimeAuth returns the auth: block of whichever
// content-source.yaml runtime resolution would use, following the same
// discovery order as ResolveRuntimeCollections
// (--content-source/MEERKAT_CONTENT_SOURCE, the user config dir, then
// the working directory).
//
// A missing file, or a file with no auth: block, returns (nil, nil):
// "no auth configured" is the back-compat state, not an error.
func LoadRuntimeAuth(contentSourceFlag string) (*authz.Config, error) {
	path, err := LocateRuntime(ResolveFlag(contentSourceFlag))
	if err != nil || path == "" {
		return nil, err
	}
	cfg, err := LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("content-source.yaml (%s): %w", path, err)
	}
	return cfg.Auth, nil
}
