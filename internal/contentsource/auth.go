package contentsource

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
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

// decodeStrict decodes body into v refusing any key v has no field for.
// A mistyped key in an auth: block (`provider:` for `providers:`) would
// otherwise be dropped silently and leave the server running with less
// policy than the file appears to state.
func decodeStrict(body []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// checkAuthSubtree strictly decodes just the auth: subtree of a
// content-source.yaml. The rest of that document keeps its lenient
// decoding (older files may carry keys newer or older builds ignore),
// but the policy block fails closed on an unknown key.
func checkAuthSubtree(body []byte) error {
	var raw struct {
		Auth yaml.Node `yaml:"auth"`
	}
	if err := yaml.Unmarshal(body, &raw); err != nil || raw.Auth.Kind == 0 {
		return nil // the main decode reports a malformed document
	}
	sub, err := yaml.Marshal(&raw)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	var doc authDocument
	if err := decodeStrict(sub, &doc); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	return nil
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
	if err := checkAuthSubtree(body); err != nil {
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

// LoadRuntimeObservability returns the observability: block of whichever
// content-source.yaml runtime resolution would use, following the same
// discovery order LoadRuntimeAuth does.
//
// A missing file, or a file with no observability: block, returns
// (nil, nil). That is the back-compat state and the overwhelmingly
// common one: no block means no OpenTelemetry SDK is constructed at all.
// It sits beside LoadRuntimeAuth rather than inside it because the two
// answer different questions for different subcommands, and a caller
// that wants one should not be made to think about the other.
// LoadRuntimeSessions returns the `sessions:` block of the runtime
// content-source.yaml, or nil when there is none.
func LoadRuntimeSessions(contentSourceFlag string) (*SessionsSpec, error) {
	path, err := LocateRuntime(ResolveFlag(contentSourceFlag))
	if err != nil || path == "" {
		return nil, err
	}
	cfg, err := LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("content-source.yaml (%s): %w", path, err)
	}
	return cfg.Sessions, nil
}

// LoadRuntimeCache returns the `cache:` block of the runtime
// content-source.yaml, or nil when there is none.
func LoadRuntimeCache(contentSourceFlag string) (*CacheSpec, error) {
	path, err := LocateRuntime(ResolveFlag(contentSourceFlag))
	if err != nil || path == "" {
		return nil, err
	}
	cfg, err := LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("content-source.yaml (%s): %w", path, err)
	}
	return cfg.Cache, nil
}

// LoadRuntimeIntake returns the `intake:` block of the runtime
// content-source.yaml, or nil when there is none.
func LoadRuntimeIntake(contentSourceFlag string) (*memory.Spec, error) {
	path, err := LocateRuntime(ResolveFlag(contentSourceFlag))
	if err != nil || path == "" {
		return nil, err
	}
	cfg, err := LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("content-source.yaml (%s): %w", path, err)
	}
	return cfg.Intake, nil
}

func LoadRuntimeObservability(contentSourceFlag string) (*telemetry.Config, error) {
	path, err := LocateRuntime(ResolveFlag(contentSourceFlag))
	if err != nil || path == "" {
		return nil, err
	}
	cfg, err := LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("content-source.yaml (%s): %w", path, err)
	}
	return cfg.Observability, nil
}
