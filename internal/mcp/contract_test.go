package mcp

// mk_list_collections is the surface where the update-contract layer
// (internal/collections/contract.go) meets a caller: these tests pin
// the wire shape of the embedded contract and that it is rendered
// per caller — the same declared contract must read differently for a
// publisher and for a read-only caller.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// contractRegistry mounts one collection with a merge-request contract,
// one with a direct contract, and one with no update: block at all.
func contractRegistry(t *testing.T) *collections.Registry {
	t.Helper()
	handbook := collections.FromPages("handbook", []kb.Page{
		testPage("conventions/naming", "Naming", "body", "concepts", "reviewed", "team-a"),
	})
	handbook.Source = contentsource.Source{
		Type:        contentsource.TypeGCS,
		Bucket:      "my-org-knowledge",
		Object:      "bundles/handbook-v3.tar.gz",
		Description: "Engineering handbook.",
		Update: &contentsource.UpdateSpec{
			Method:       contentsource.UpdateMergeRequest,
			Repo:         "https://github.com/example-org/handbook.git",
			Host:         contentsource.UpdateHostGitHub,
			Branch:       "main",
			Path:         "wiki",
			Instructions: "Fork, branch, open a PR against main.",
		},
	}
	scratch := collections.FromPages("scratch", []kb.Page{
		testPage("notes/today", "Today", "body", "notes", "draft", "team-a"),
	})
	scratch.Source = contentsource.Source{
		Type: contentsource.TypeLocal,
		Path: "../scratch",
		Update: &contentsource.UpdateSpec{
			Method: contentsource.UpdateDirect,
		},
	}
	plain := collections.FromPages("plain", []kb.Page{
		testPage("adr/0001", "ADR 1", "body", "adr", "reviewed", "team-b"),
	})
	reg, err := collections.New(handbook, scratch, plain)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// entriesByName drives the real handler and indexes the decoded wire
// entries by collection name.
func entriesByName(t *testing.T, ctx context.Context, reg *collections.Registry) map[string]map[string]any {
	t.Helper()
	res, err := listCollectionsHandler(reg)(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &entries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	byName := make(map[string]map[string]any, len(entries))
	for _, e := range entries {
		byName[e["name"].(string)] = e
	}
	return byName
}

// TestListCollections_ContractShape: with no grants in force (stdio /
// unauthenticated), the declared contract is rendered verbatim, the
// description rides at the top level, and a collection without an
// update: block has no contract key at all — method none is a
// statement, not an empty object.
func TestListCollections_ContractShape(t *testing.T) {
	byName := entriesByName(t, context.Background(), contractRegistry(t))

	hb := byName["handbook"]
	if hb["description"] != "Engineering handbook." {
		t.Errorf("description = %v, want the declared one", hb["description"])
	}
	contract, ok := hb["contract"].(map[string]any)
	if !ok {
		t.Fatalf("handbook contract missing or not an object: %v", hb["contract"])
	}
	want := map[string]string{
		"method":          "merge-request",
		"declared_method": "merge-request",
		"repo":            "https://github.com/example-org/handbook.git",
		"host":            "github",
		"branch":          "main",
		"path":            "wiki",
		"instructions":    "Fork, branch, open a PR against main.",
	}
	for k, v := range want {
		if contract[k] != v {
			t.Errorf("contract[%q] = %v, want %q", k, contract[k], v)
		}
	}

	if _, present := byName["plain"]["contract"]; present {
		t.Errorf("plain declared no contract but one was rendered: %v", byName["plain"]["contract"])
	}
}

// TestListCollections_ContractIsPerCaller: the same direct-declared
// collection must render as "direct" for a caller holding the publish
// capability and demote — with a reason — for a caller who only reads.
func TestListCollections_ContractIsPerCaller(t *testing.T) {
	reg := contractRegistry(t)

	caller := func(caps ...authz.Capability) context.Context {
		g := authz.NewGrants(
			authz.Identity{Subject: "user-1", Issuer: "https://idp.example.com"},
			map[string][]authz.Capability{"scratch": caps},
		)
		return authz.NewContext(context.Background(), g)
	}

	publisher := entriesByName(t, caller(authz.CapRead, authz.CapGlobalWrite), reg)["scratch"]
	pc, ok := publisher["contract"].(map[string]any)
	if !ok {
		t.Fatalf("publisher sees no contract: %v", publisher)
	}
	if pc["method"] != "direct" || pc["declared_method"] != "direct" {
		t.Errorf("publisher contract = %v/%v, want direct/direct", pc["method"], pc["declared_method"])
	}

	reader := entriesByName(t, caller(authz.CapRead), reg)["scratch"]
	rc, ok := reader["contract"].(map[string]any)
	if !ok {
		t.Fatalf("reader sees no contract: %v", reader)
	}
	if rc["method"] == "direct" {
		t.Errorf("read-only caller was told to write directly: %v", rc)
	}
	if rc["declared_method"] != "direct" {
		t.Errorf("declared_method = %v, want direct regardless of caller", rc["declared_method"])
	}
	if reason, _ := rc["reason"].(string); reason == "" {
		t.Errorf("demoted contract carries no reason: %v", rc)
	}
}
