package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCmd_Text(t *testing.T) {
	cmd := newVersionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(out.String(), "meerkat ") {
		t.Errorf("text output missing 'meerkat' banner: %q", out.String())
	}
	if !strings.Contains(out.String(), "runtime:") {
		t.Errorf("text output missing runtime line: %q", out.String())
	}
}

func TestVersionCmd_JSON(t *testing.T) {
	cmd := newVersionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version --json: %v", err)
	}
	var info versionInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatalf("version --json not valid JSON: %v\n%s", err, out.String())
	}
	if info.GoVersion == "" || info.OS == "" || info.Arch == "" {
		t.Errorf("runtime fields not populated: %+v", info)
	}
}

func TestVersionEq(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.2.3", "1.2.3", true},
		{"1.2.3-dirty", "1.2.3", true},
		{"v1.2.3-dirty", "1.2.3", true},
		{"1.2.3", "1.2.4", false},
		{"v2.0.0", "v2.0.0", true},
	}
	for _, tc := range cases {
		if got := versionEq(tc.a, tc.b); got != tc.want {
			t.Errorf("versionEq(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
