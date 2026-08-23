package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// mcp_test.go covers root.go's wiring of the auth: block: that it
// reaches `mk mcp serve-http` through PersistentPreRunE, that --kb-dir
// suppresses it (the same rule that governs content), and that a bad
// policy fails the command rather than being ignored.

// testSentinelAuth is a non-nil value assigned to activeAuth before a
// run that is expected to CLEAR it, so "left unset" is distinguishable
// from "never written".
var testSentinelAuth = authz.Config{Resource: "https://sentinel.invalid"}

const testAuthBlock = `
auth:
  resource: https://mcp.example.com/mcp
  providers:
    - issuer: https://login.example.com/tenant/v2.0
      audience: api://meerkat
  rules:
    - name: sre
      groups: [sre]
      collections: [runbooks]
`

// runRoot executes the real cobra tree with args and returns
// stdout+stderr and the error.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// writeConfig writes a content-source.yaml plus a minimal content tree
// and returns the config path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	kb := filepath.Join(dir, "kb", "wiki")
	if err := os.MkdirAll(kb, 0o750); err != nil {
		t.Fatal(err)
	}
	page := "---\nid: index\ntitle: Index\n---\n# Index\n\nbody\n"
	if err := os.WriteFile(filepath.Join(kb, "index.md"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "content-source.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServeHTTPCommand_IsRegistered(t *testing.T) {
	out, err := runRoot(t, "mcp", "--help")
	if err != nil {
		t.Fatalf("mcp --help: %v", err)
	}
	if !strings.Contains(out, "serve-http") {
		t.Errorf("mcp --help should list serve-http:\n%s", out)
	}

	out, err = runRoot(t, "mcp", "serve-http", "--help")
	if err != nil {
		t.Fatalf("mcp serve-http --help: %v", err)
	}
	for _, want := range []string{"--auth-config", "--path", "--stateful", "--trust-proxy-host", "/readyz", "/metrics"} {
		if !strings.Contains(out, want) {
			t.Errorf("serve-http --help missing %q:\n%s", want, out)
		}
	}
}

func TestPersistentPreRun_LoadsAuthBlock(t *testing.T) {
	path := writeConfig(t, "content:\n  type: local\n  path: ./kb\n"+testAuthBlock)

	activeAuth = nil
	// `list` is enough: PersistentPreRunE runs for every subcommand.
	if _, err := runRoot(t, "--content-source", path, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if activeAuth == nil {
		t.Fatal("the auth: block should be resolved once per invocation, for whichever subcommand runs")
	}
	if len(activeAuth.Rules) != 1 || activeAuth.Rules[0].Name != "sre" {
		t.Errorf("activeAuth = %+v", activeAuth)
	}
	if !activeAuth.Enabled() {
		t.Error("a config with providers should be enabled")
	}
}

func TestPersistentPreRun_NoAuthBlockLeavesPolicyUnset(t *testing.T) {
	path := writeConfig(t, "content:\n  type: local\n  path: ./kb\n")

	activeAuth = &testSentinelAuth
	if _, err := runRoot(t, "--content-source", path, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if activeAuth != nil {
		t.Fatalf("a config with no auth: block must leave the policy unset, got %+v", activeAuth)
	}
}

func TestPersistentPreRun_KBDirSuppressesAuthDiscovery(t *testing.T) {
	// A content-source.yaml with an auth: block sits in the working
	// directory, but --kb-dir wins outright and must not pick it up:
	// the same rule that governs content resolution.
	dir := t.TempDir()
	kb := filepath.Join(dir, "wiki")
	if err := os.MkdirAll(kb, 0o750); err != nil {
		t.Fatal(err)
	}
	page := "---\nid: index\ntitle: Index\n---\n# Index\n\nbody\n"
	if err := os.WriteFile(filepath.Join(kb, "index.md"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content-source.yaml"),
		[]byte("content:\n  type: local\n  path: ./wiki\n"+testAuthBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	activeAuth = &testSentinelAuth
	if _, err := runRoot(t, "--kb-dir", dir, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if activeAuth != nil {
		t.Fatalf("--kb-dir must suppress auth: discovery, got %+v", activeAuth)
	}
}

func TestPersistentPreRun_InvalidAuthBlockFailsTheCommand(t *testing.T) {
	path := writeConfig(t, `
content:
  type: local
  path: ./kb
auth:
  resource: https://mcp.example.com/mcp
  providers: [{issuer: "https://i.example.com", audience: a}]
  rules:
    - name: oops
      collections: [runbooks]
      capabilities: [write]
`)
	_, err := runRoot(t, "--content-source", path, "list")
	if err == nil {
		t.Fatal("an invalid auth: block must fail the command, not be ignored")
	}
	if !strings.Contains(err.Error(), `unknown capability "write"`) {
		t.Errorf("err = %v, want it to name the bad capability", err)
	}
}

func TestServeHTTPCommand_RejectsAMissingAuthConfig(t *testing.T) {
	_, err := runRoot(t, "mcp", "serve-http", "--auth-config", filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("a --auth-config path that does not exist must be an error")
	}
}
