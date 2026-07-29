package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// withListFixture points kb.List at a small, deterministic set of pages
// for the duration of the test, independent of whatever content happens
// to be embedded in this build.
func withListFixture(t *testing.T) {
	t.Helper()
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md":    {Data: []byte("---\nid: tables/orders\ntitle: Orders\ntype: BigQuery Table\nstatus: stable\n---\n# Orders\n")},
		"content/tables/customers.md": {Data: []byte("---\nid: tables/customers\ntitle: Customers\ntype: BigQuery Table\nstatus: draft\n---\n# Customers\n")},
		"content/playbooks/oncall.md": {Data: []byte("---\nid: playbooks/oncall\ntitle: Oncall\ntype: Playbook\nstatus: reviewed\n---\n# Oncall\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })
}

// runList executes `mk list` with args against whatever kb.UseFS
// currently points at, and returns stdout/stderr.
func runList(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := newListCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out.String(), errOut.String()
}

// TestListCmd_TypeFilter proves --type (OKF's concept-kind field,
// SPEC.md §4.1) filters mk list's default output the same way
// --category/--status/--owner already do.
func TestListCmd_TypeFilter(t *testing.T) {
	withListFixture(t)

	out, _ := runList(t, "--type", "BigQuery Table")
	if !strings.Contains(out, "tables/orders") || !strings.Contains(out, "tables/customers") {
		t.Errorf("expected both BigQuery Table pages, got: %s", out)
	}
	if strings.Contains(out, "playbooks/oncall") {
		t.Errorf("--type filter leaked a Playbook page: %s", out)
	}
}

// TestListCmd_TypeFilterNoMatches proves an unmatched --type value
// yields zero rows rather than an error (consistent with the existing
// category/status/owner filters).
func TestListCmd_TypeFilterNoMatches(t *testing.T) {
	withListFixture(t)

	out, _ := runList(t, "--type", "Nonexistent")
	for _, id := range []string{"tables/orders", "tables/customers", "playbooks/oncall"} {
		if strings.Contains(out, id) {
			t.Errorf("--type Nonexistent should match nothing, got: %s", out)
		}
	}
}

// TestListCmd_TypeAndStatusCompose proves --type composes (AND) with
// the pre-existing filters, matching the documented "filters compose"
// contract.
func TestListCmd_TypeAndStatusCompose(t *testing.T) {
	withListFixture(t)

	out, _ := runList(t, "--type", "BigQuery Table", "--status", "draft")
	if !strings.Contains(out, "tables/customers") {
		t.Errorf("expected tables/customers (type=BigQuery Table, status=draft), got: %s", out)
	}
	if strings.Contains(out, "tables/orders") {
		t.Errorf("tables/orders has status=stable, should be excluded by --status draft: %s", out)
	}
}

// TestListCmd_JSONIncludesType proves --json carries the new `type`
// field alongside the pre-existing category/status/owner/source.
func TestListCmd_JSONIncludesType(t *testing.T) {
	withListFixture(t)

	out, _ := runList(t, "--json")
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	found := false
	for _, e := range entries {
		if e["id"] == "tables/orders" {
			found = true
			if e["type"] != "BigQuery Table" {
				t.Errorf("type = %v, want %q", e["type"], "BigQuery Table")
			}
		}
	}
	if !found {
		t.Fatal("tables/orders not found in JSON output")
	}
}

// TestListCmd_OKFStatusValuesFilter proves OKF's own status vocabulary
// (draft/stable/deprecated) filters through --status exactly like
// meerkat's placeholder/reviewed/... — no translation layer, per the
// "status: coexist, don't map" decision (kb.Frontmatter.Status).
func TestListCmd_OKFStatusValuesFilter(t *testing.T) {
	withListFixture(t)

	out, _ := runList(t, "--status", "draft")
	if !strings.Contains(out, "tables/customers") {
		t.Errorf("--status draft should match tables/customers, got: %s", out)
	}
	if strings.Contains(out, "tables/orders") || strings.Contains(out, "playbooks/oncall") {
		t.Errorf("--status draft should only match tables/customers, got: %s", out)
	}
}
