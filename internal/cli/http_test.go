package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeKeyFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveAPIKey(t *testing.T) {
	const k1, k2 = "0123456789abcdef-one", "0123456789abcdef-two"
	file := writeKeyFile(t, "  "+k1+"\n", 0o600)

	for _, tc := range []struct {
		name, env, file, flag string
		want                  string
		wantWarn              string
		wantErr               string
	}{
		{name: "file, trimmed", file: file, want: k1},
		{name: "env", env: k2, want: k2},
		{name: "flag works but warns", flag: k2, want: k2, wantWarn: "process list"},
		{name: "env beats file", env: k2, file: file, want: k2, wantWarn: "overrides"},
		{name: "nothing", wantErr: "no API key configured"},
		{name: "missing file", file: filepath.Join(t.TempDir(), "nope"), wantErr: "--api-key-file"},
		{name: "empty file", file: writeKeyFile(t, " \n", 0o600), wantErr: "is empty"},
		{name: "directory", file: t.TempDir(), wantErr: "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warns, err := resolveAPIKey(tc.env, tc.file, tc.flag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("key = %q, want %q", got, tc.want)
			}
			if tc.wantWarn != "" && !strings.Contains(strings.Join(warns, "\n"), tc.wantWarn) {
				t.Errorf("warnings = %v, want one containing %q", warns, tc.wantWarn)
			}
			if tc.wantWarn == "" && len(warns) != 0 {
				t.Errorf("unexpected warnings %v", warns)
			}
		})
	}
}

func TestResolveAPIKey_WarnsOnWorldReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	path := writeKeyFile(t, "0123456789abcdef-one\n", 0o644)
	_, warns, err := resolveAPIKey("", path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "chmod 600") {
		t.Errorf("warnings = %v, want a chmod 600 hint", warns)
	}
}

// The command refuses a short key from any source, and the two flags
// cannot be combined.
func TestHTTPServe_APIKeyValidation(t *testing.T) {
	t.Setenv("MEERKAT_API_KEY", "")
	out, err := runRoot(t, "http", "serve", "--api-key", "short")
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("short --api-key: err = %v (output %q)", err, out)
	}
	file := writeKeyFile(t, "short\n", 0o600)
	if _, err := runRoot(t, "http", "serve", "--api-key-file", file); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("short --api-key-file: err = %v", err)
	}
	if _, err := runRoot(t, "http", "serve", "--api-key", "x", "--api-key-file", file); err == nil ||
		!strings.Contains(err.Error(), "none of the others can be") {
		t.Errorf("combined flags: err = %v", err)
	}
}
