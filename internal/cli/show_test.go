package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// runShow executes `mk show` with args and returns stdout.
func runShow(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out.String()
}

// TestShowCmd_JSONIncludesTrustTierAndStale proves `mk show --json`
// carries the OKF-derived trust_tier/stale signals (kb.Frontmatter.
// TrustTier / IsStale) alongside the promoted type/description core
// fields, for a concept with a human: verifier and a past stale_after.
func TestShowCmd_JSONIncludesTrustTierAndStale(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md": {Data: []byte(`---
id: tables/orders
title: Orders
type: BigQuery Table
description: One row per order.
status: stable
stale_after: 2020-01-01
verified:
  - { by: human:ahormati, at: 2026-06-25T09:00:00Z }
---
# Orders
`)},
	})
	t.Cleanup(func() { kb.UseFS(nil) })

	out := runShow(t, "tables/orders", "--json")
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if result["trust_tier"] != "human-reviewed" {
		t.Errorf("trust_tier = %v, want human-reviewed", result["trust_tier"])
	}
	if result["stale"] != true {
		t.Errorf("stale = %v, want true (stale_after 2020-01-01 is in the past)", result["stale"])
	}
	front, ok := result["front"].(map[string]any)
	if !ok {
		t.Fatalf("front is not an object: %v", result["front"])
	}
	if front["type"] != "BigQuery Table" {
		t.Errorf("front.type = %v", front["type"])
	}
	if front["description"] != "One row per order." {
		t.Errorf("front.description = %v", front["description"])
	}
}

// TestShowCmd_JSONUnverifiedAndNotStale covers the opposite corner: no
// verified key and no stale_after at all.
func TestShowCmd_JSONUnverifiedAndNotStale(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/customers.md": {Data: []byte("---\nid: tables/customers\ntype: BigQuery Table\n---\n# Customers\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })

	out := runShow(t, "tables/customers", "--json")
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if result["trust_tier"] != "unverified" {
		t.Errorf("trust_tier = %v, want unverified", result["trust_tier"])
	}
	if result["stale"] != false {
		t.Errorf("stale = %v, want false", result["stale"])
	}
}

// TestShowCmd_TextOutputUnaffected proves the non-JSON default output
// is still exactly the page body — the trust_tier/stale addition is
// --json-only.
func TestShowCmd_TextOutputUnaffected(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md": {Data: []byte("---\nid: tables/orders\ntype: BigQuery Table\n---\n# Orders\n\nBody text.\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })

	out := runShow(t, "tables/orders")
	if strings.Contains(out, "trust_tier") {
		t.Errorf("plain-text output should not mention trust_tier: %q", out)
	}
	if !strings.Contains(out, "Body text.") {
		t.Errorf("expected the page body, got: %q", out)
	}
}
