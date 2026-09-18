package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

type outcomeFixture struct {
	reg    *collections.Registry
	opts   transportOptions
	logDir string
	intake string
}

func newOutcomeFixture(t *testing.T) outcomeFixture {
	t.Helper()
	reg, err := collections.New(collections.FromPages("flux", []kb.Page{{ID: "concepts/drift", Title: "Drift", Body: "flux helmrelease drift"}}))
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	log, err := traversal.Open(context.Background(), &traversal.Config{Backend: "local", Path: logDir, HMACKeyEnv: "MEERKAT_TEST_PATH_KEY"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	intakeDir := filepath.Join(t.TempDir(), "intake")
	store, err := memory.OpenLocal(intakeDir)
	if err != nil {
		t.Fatal(err)
	}
	return outcomeFixture{reg: reg, opts: transportOptions{Outcome: OutcomeOptions{Log: log, Intake: store}}, logDir: logDir, intake: intakeDir}
}

func callOutcome(t *testing.T, ctx context.Context, f outcomeFixture, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = toolReportOutcome
	req.Params.Arguments = args
	res, err := reportOutcomeHandler(f.reg, f.opts)(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if res.IsError {
		return map[string]any{"error": text}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("response %q: %v", text, err)
	}
	return out
}

func logLines(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(dir, "telemetry", "paths"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			b, _ := os.ReadFile(p)
			out = append(out, string(b))
		}
		return nil
	})
	return out
}

func intakeFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			b, _ := os.ReadFile(p)
			out = append(out, string(b))
		}
		return nil
	})
	return out
}

func TestReportOutcome_GaveUpWithFallbackWritesIntakeAndLog(t *testing.T) {
	f := newOutcomeFixture(t)
	out := callOutcome(t, context.Background(), f, map[string]any{
		"session_id": "sess-42", "outcome": "gave_up", "initial_query": "how do I rotate the datadog api key",
		"attempted": []any{"flux", "vendors"}, "pages": []any{},
		"quality":  map[string]any{"accuracy": 0.0, "completeness": 0.0, "answer_quality": 0.0},
		"fallback": map[string]any{"kind": "web", "summary": "Rotate it in Organization Settings > API Keys; revoke the old one after deploy.", "sources": []any{"https://docs.datadoghq.com/account_management/api-app-keys/"}},
	})
	if out["recorded"] != true || out["logged"] != true || out["intake"] != "written" || out["intake_id"] == nil {
		t.Fatalf("response = %v", out)
	}
	pages := intakeFiles(t, f.intake)
	if len(pages) != 1 {
		t.Fatalf("intake objects = %d, want 1", len(pages))
	}
	page := pages[0]
	for _, want := range []string{`"type":"research-raw"`, `"status":"unverified"`, `"source":"agent-fallback"`, `"question":"how do I rotate the datadog api key"`, `"attempted":["flux","vendors"]`, "Rotate it in Organization Settings", "## Sources", "docs.datadoghq.com"} {
		if !strings.Contains(page, want) {
			t.Errorf("intake page lacks %q:\n%s", want, page)
		}
	}
	if !strings.HasPrefix(page, "---\n{") {
		t.Errorf("intake page must open with a JSON frontmatter block:\n%s", page)
	}
	lines := logLines(t, f.logDir)
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	line := lines[0]
	if strings.Contains(line, "sess-42") || strings.Contains(line, `"flux"`) || strings.Contains(line, `"vendors"`) {
		t.Errorf("plaintext identity in the traversal log: %s", line)
	}
	if !strings.Contains(line, "how do I rotate the datadog api key") || !strings.Contains(line, `"outcome":"gave_up"`) || !strings.Contains(line, `"intake_id":"raw/`) || !strings.Contains(line, `"hops":2`) {
		t.Errorf("log line lacks the query, outcome, intake id or hops: %s", line)
	}
}

func TestReportOutcome_FoundWritesLogOnly(t *testing.T) {
	f := newOutcomeFixture(t)
	out := callOutcome(t, context.Background(), f, map[string]any{
		"outcome": "found", "initial_query": "flux drift", "pages": []any{"flux:concepts/drift"}, "attempted": []any{"flux"},
		"quality": map[string]any{"accuracy": 1, "completeness": 0.9, "answer_quality": 0.8, "notes": "spot on"},
	})
	if out["recorded"] != true || out["logged"] != true || out["intake"] != "none" || out["intake_id"] != nil {
		t.Fatalf("response = %v", out)
	}
	if n := len(intakeFiles(t, f.intake)); n != 0 {
		t.Errorf("a found report wrote %d intake objects", n)
	}
	lines := logLines(t, f.logDir)
	if len(lines) != 1 || !strings.Contains(lines[0], `"answer_quality":0.8`) || strings.Contains(lines[0], "concepts/drift") {
		t.Errorf("log = %v", lines)
	}
}

func TestReportOutcome_ValidationAndGating(t *testing.T) {
	f := newOutcomeFixture(t)
	ctx := context.Background()
	bad := []map[string]any{
		{"outcome": "maybe"},
		{},
		{"outcome": "found", "session_id": "has space"},
		{"outcome": "found", "quality": map[string]any{"accuracy": 2.0, "completeness": 0, "answer_quality": 0}},
		{"outcome": "found", "quality": map[string]any{"accuracy": 0.5}},
		{"outcome": "gave_up", "fallback": map[string]any{"kind": "carrier-pigeon"}},
		{"outcome": "gave_up", "fallback": map[string]any{"kind": "web"}},
		{"outcome": "gave_up", "fallback": map[string]any{"kind": "web", "summary": "x", "sources": []any{"ftp://nope"}}},
		{"outcome": "found", "pages": "not-a-list"},
		{"outcome": "found", "initial_query": strings.Repeat("q", maxInitialQueryBytes+1)},
	}
	for i, args := range bad {
		if out := callOutcome(t, ctx, f, args); out["error"] == nil {
			t.Errorf("case %d accepted: %v", i, out)
		}
	}
	if n := len(logLines(t, f.logDir)) + len(intakeFiles(t, f.intake)); n != 0 {
		t.Errorf("rejected reports wrote %d objects", n)
	}

	// A caller without intake-write may report but may not deposit.
	g := authz.NewGrants(authz.Identity{Subject: "tester", Issuer: "https://issuer.example"}, map[string][]authz.Capability{"flux": {authz.CapRead}})
	out := callOutcome(t, authz.NewContext(ctx, g), f, map[string]any{
		"outcome": "gave_up", "fallback": map[string]any{"kind": "source", "summary": "read the controller code"},
	})
	if out["recorded"] != true || out["intake"] != "not_permitted" || out["logged"] != true {
		t.Errorf("read-only caller: %v", out)
	}
	if n := len(intakeFiles(t, f.intake)); n != 0 {
		t.Errorf("read-only caller wrote %d intake objects", n)
	}
	g = authz.NewGrants(authz.Identity{Subject: "tester", Issuer: "https://issuer.example"}, map[string][]authz.Capability{"flux": {authz.CapRead, authz.CapIntakeWrite}})
	out = callOutcome(t, authz.NewContext(ctx, g), f, map[string]any{
		"outcome": "gave_up", "fallback": map[string]any{"kind": "source", "summary": "read the controller code"},
	})
	if out["intake"] != "written" {
		t.Errorf("intake-write caller: %v", out)
	}

	// Unconfigured sinks: telemetry only, and the response says so.
	bare := outcomeFixture{reg: f.reg, opts: transportOptions{}}
	out = callOutcome(t, ctx, bare, map[string]any{"outcome": "gave_up", "fallback": map[string]any{"kind": "web", "summary": "s"}})
	if out["recorded"] != true || out["logged"] != false || out["intake"] != "not_configured" {
		t.Errorf("unconfigured: %v", out)
	}
}
