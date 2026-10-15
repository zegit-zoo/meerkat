package mcp

import (
	"context"
	"strings"
	"testing"
)

// A deposit may name only https sources on public hosts; anything else
// is refused before it is written, so it can never reach a researcher
// prompt (meerkat-mob#34).
func TestReportOutcome_RefusesNonPublicSources(t *testing.T) {
	f := newOutcomeFixture(t)
	ctx := context.Background()
	for _, src := range []string{
		"file:///home/op/.aws/credentials",
		"git://example.com/r.git",
		"http://example.com/doc",
		"https://169.254.169.254/latest/meta-data/",
		"https://127.0.0.1:8080/",
		"https://[::1]/",
		"https://10.1.2.3/",
		"https://localhost/x",
	} {
		out := callOutcome(t, ctx, f, map[string]any{
			"outcome":  "gave_up",
			"fallback": map[string]any{"kind": "web", "summary": "s", "sources": []any{"https://example.com/ok", src}},
		})
		msg, _ := out["error"].(string)
		if msg == "" || !strings.Contains(msg, "only https URLs to public hosts") {
			t.Errorf("source %q: response %v; want a refusal", src, out)
		}
	}
	if n := len(intakeFiles(t, f.intake)); n != 0 {
		t.Errorf("refused deposits wrote %d intake objects", n)
	}
	out := callOutcome(t, ctx, f, map[string]any{
		"outcome":  "gave_up",
		"fallback": map[string]any{"kind": "web", "summary": "s", "sources": []any{"https://example.com/ok"}},
	})
	if out["error"] != nil || out["intake"] != "written" {
		t.Errorf("an https public source was refused: %v", out)
	}
}
