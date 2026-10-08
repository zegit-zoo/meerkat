package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

func TestCheckUnauthenticatedBind(t *testing.T) {
	oidc := &authz.Config{Resource: "https://m.example.com", Providers: []authz.Provider{{Issuer: "https://i.example.com"}}}
	gateway := &authz.Config{AllowUnauthenticated: true}
	for _, tc := range []struct {
		name    string
		bind    string
		auth    *authz.Config
		insec   bool
		wantErr bool
	}{
		{"loopback default", "127.0.0.1", nil, false, false},
		{"empty host is the loopback default", "", nil, false, false},
		{"localhost", "localhost", nil, false, false},
		{"ipv6 loopback", "::1", nil, false, false},
		{"loopback with port (activated socket)", "127.0.0.1:4005", nil, false, false},
		{"all interfaces, no auth", "0.0.0.0", nil, false, true},
		{"ipv6 any, no auth", "[::]:4005", nil, false, true},
		{"routable host, no auth", "10.1.2.3", nil, false, true},
		{"hostname, no auth", "mcp.example.com", nil, false, true},
		{"all interfaces, explicit flag", "0.0.0.0", nil, true, false},
		{"all interfaces, oidc", "0.0.0.0", oidc, false, false},
		{"all interfaces, in-file gateway opt-in", "0.0.0.0", gateway, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUnauthenticatedBind(tc.bind, tc.auth, tc.insec)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--insecure-no-auth") {
				t.Errorf("the refusal must name the opt-in flag: %v", err)
			}
		})
	}
}

// TestServeHTTP_RefusesPublicBindWithoutAuth drives the real command: it
// must fail before serving, not after.
func TestServeHTTP_RefusesPublicBindWithoutAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o750); err != nil {
		t.Fatal(err)
	}
	page := "---\nid: index\ntitle: Index\n---\n# Index\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "wiki", "index.md"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	out, err := runRoot(t, "--kb-dir", dir, "mcp", "serve-http", "--host", "0.0.0.0", "--port", "0")
	if err == nil || !strings.Contains(err.Error(), "refusing to serve") {
		t.Fatalf("err = %v (output %q), want a refusal", err, out)
	}
}

// TestServeHTTP_WarnsWhenKBDirSuppressesAuth: --kb-dir drops the
// content-source.yaml auth: block; the operator is told.
func TestServeHTTP_WarnsWhenKBDirSuppressesAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o750); err != nil {
		t.Fatal(err)
	}
	page := "---\nid: index\ntitle: Index\n---\n# Index\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "wiki", "index.md"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content-source.yaml"),
		[]byte("content:\n  type: local\n  path: ./wiki\n"+testAuthBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// A content-source.yaml counts only when it is named (meerkat-mob#30),
	// so name it; --kb-dir then wins and the auth: block is suppressed.
	t.Setenv("MEERKAT_CONTENT_SOURCE", filepath.Join(dir, "content-source.yaml"))
	// The bind is public with no auth, so the command stops right after
	// the warning without needing to serve.
	out, _ := runRoot(t, "--kb-dir", dir, "mcp", "serve-http", "--host", "0.0.0.0", "--port", "0")
	if !strings.Contains(out, "auth: block in content-source.yaml is IGNORED") {
		t.Errorf("no warning about the suppressed auth: block; output %q", out)
	}
}
