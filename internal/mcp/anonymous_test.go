package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpapi "github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// anonymous_test.go drives `anonymous: true` rules (#36) end to end over
// the wire: a real mcp-go client with no Authorization header at all,
// against a real hosted server in front of a real fake OIDC issuer.
//
// The invariant every test here circles is that the anonymous caller is
// an ORDINARY restricted caller. Nothing about their request takes a
// different code path once the gate has synthesized their grants, so the
// whole of #9's invisibility property and the whole of #27's ownership
// property have to hold for them without a line of new enforcement — and
// these tests are how we know they do rather than hope so.

// publicRules publishes `runbooks` to anonymous callers and grants the
// hr group `secrets`, over the standard three-collection registry.
// `architecture` is granted to nobody at all, so a test can tell "hidden
// from this caller" from "hidden from everyone".
func publicRules() []authz.Rule {
	return []authz.Rule{
		{Name: "public", Anonymous: true, Collections: []string{"runbooks"}, Capabilities: []string{"read"}},
		{Name: "hr", Groups: []string{"hr"}, Collections: []string{"secrets"}},
	}
}

// --- the gate, over the wire ------------------------------------------

func TestHostedAnonymous_NoTokenIsAdmittedWhenSomethingIsPublished(t *testing.T) {
	f := newHostedFixture(t, publicRules())
	ctx := context.Background()

	c := f.client(ctx, "") // no Authorization header
	body, isErr := callText(t, ctx, c, toolList, map[string]any{})
	if isErr {
		t.Fatalf("mk_list failed for an anonymous caller: %s", body)
	}
	if !strings.Contains(body, "incidents/paging") {
		t.Errorf("the published collection's pages should be readable: %s", body)
	}
	if !f.srv.AnonymousEnabled() {
		t.Error("AnonymousEnabled() should be true")
	}
	if !f.srv.AuthEnabled() {
		t.Error("AuthEnabled() should stay true — anonymous access is a carve-out, not a posture change")
	}
}

func TestHostedAnonymous_ExpiredTokenIs401NotAnonymous(t *testing.T) {
	// Over the wire, against a server that WOULD have admitted a caller
	// carrying no header at all. The status code is the whole test.
	f := newHostedFixture(t, publicRules())

	resp := f.do(t, http.MethodPost, f.srv.EndpointPath(), f.expiredToken("alice"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — an expired token must not be downgraded to anonymous", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
	}
	body := readAll(t, resp)
	for _, name := range []string{"runbooks", "architecture", "secrets"} {
		if strings.Contains(body, name) {
			t.Errorf("a 401 must not name a collection (%q leaked): %s", name, body)
		}
	}
}

func TestHostedAnonymous_ForgedTokenIs401NotAnonymous(t *testing.T) {
	f := newHostedFixture(t, publicRules())
	forged := f.issuer.TokenSignedByOther(t, testClaims("mallory"))

	resp := f.do(t, http.MethodPost, f.srv.EndpointPath(), forged)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a forged token must not be downgraded to anonymous", resp.StatusCode)
	}
}

// TestHostedAnonymous_MetadataAndChallengeAreUnchanged pins the RFC 9728
// half: publishing a collection does not remove the discovery loop for
// the clients that still need a token.
func TestHostedAnonymous_MetadataAndChallengeAreUnchanged(t *testing.T) {
	f := newHostedFixture(t, publicRules())

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		resp := f.do(t, http.MethodGet, path, "")
		body := readAll(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		if !strings.Contains(body, testResource) {
			t.Errorf("GET %s body = %s, want the resource identifier", path, body)
		}
	}
	// And a bad token still gets a challenge pointing there.
	resp := f.do(t, http.MethodPost, f.srv.EndpointPath(), "not-a-jwt")
	defer resp.Body.Close()
	want := "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+want+`"`) {
		t.Errorf("WWW-Authenticate = %q, want resource_metadata pointing at %s", resp.Header.Get("WWW-Authenticate"), want)
	}
}

// --- invisibility parity ----------------------------------------------

// TestHostedAnonymous_HiddenCollectionsAreInvisibleToAnonymousCallers is
// the acceptance test for "hidden answers identically to nonexistent",
// re-run for the caller who has no identity at all. It is deliberately
// the same list of oracles TestHosted_HiddenCollectionIsInvisibleEverywhere
// walks for an authenticated caller: the anonymous path must not have
// bought itself an exemption from any of them.
func TestHostedAnonymous_HiddenCollectionsAreInvisibleToAnonymousCallers(t *testing.T) {
	f := newHostedFixture(t, publicRules())
	ctx := context.Background()
	c := f.client(ctx, "")

	t.Run("tool descriptions name only the published set", func(t *testing.T) {
		res, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		if len(res.Tools) != 4 {
			t.Fatalf("got %d tools, want the 4 read tools", len(res.Tools))
		}
		for _, tool := range res.Tools {
			blob, _ := json.Marshal(tool)
			text := string(blob)
			for _, hidden := range []string{"secrets", "architecture"} {
				if strings.Contains(text, hidden) {
					t.Errorf("tool %q names a collection an anonymous caller may not read: %s", tool.Name, text)
				}
			}
			// One visible collection: the plain, single-collection UX.
			if !strings.Contains(text, "mounts a single collection (runbooks)") {
				t.Errorf("tool %q should present as single-collection: %s", tool.Name, text)
			}
		}
	})

	t.Run("search cannot reach a hidden collection", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolSearch, map[string]any{"query": "compensation"})
		if isErr {
			t.Fatalf("mk_search failed: %s", body)
		}
		if strings.Contains(body, "payroll") || strings.Contains(body, "secrets") {
			t.Errorf("mk_search leaked a hidden hit: %s", body)
		}
	})

	t.Run("a shared page id is unambiguous for a one-collection caller", func(t *testing.T) {
		// "shared/overview" exists in all three. For a caller who sees one,
		// it resolves — silently and correctly — rather than reporting an
		// ambiguity that would count collections they cannot see.
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "shared/overview"})
		if isErr {
			t.Fatalf("mk_show failed: %s", body)
		}
		if !strings.Contains(body, "Runbook Overview") {
			t.Errorf("expected the runbooks copy: %s", body)
		}
		if strings.Contains(body, "exists in 3 collections") || strings.Contains(body, "secrets") {
			t.Errorf("the ambiguity path counted collections the caller cannot see: %s", body)
		}
	})

	t.Run("naming a hidden collection reads as unknown", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{
			"id": "payroll/salaries", "collection": "secrets",
		})
		if !isErr {
			t.Fatalf("expected an error, got: %s", body)
		}
		if !strings.Contains(body, "unknown collection") {
			t.Errorf("naming a hidden collection should read as unknown: %s", body)
		}
		if strings.Contains(body, "architecture") || strings.Contains(body, "available: runbooks, architecture") {
			t.Errorf("the available: list leaked the mounted set: %s", body)
		}
	})

	t.Run("a hidden collection and a nonexistent one answer identically", func(t *testing.T) {
		// The strongest form of the invisibility property: not "both
		// error", but "the two sentences differ by nothing except the name
		// the caller typed". Substituting each name out and comparing is
		// what makes a future error message that leaked a hint — "did you
		// mean", a different verb, a different available: list — fail here.
		hidden, hiddenErr := callText(t, ctx, c, toolShow, map[string]any{
			"id": "some/page", "collection": "secrets",
		})
		absent, absentErr := callText(t, ctx, c, toolShow, map[string]any{
			"id": "some/page", "collection": "a-collection-nobody-ever-mounted",
		})
		if !hiddenErr || !absentErr {
			t.Fatalf("both should error; hidden=%q absent=%q", hidden, absent)
		}
		h := strings.ReplaceAll(hidden, "secrets", "<name>")
		a := strings.ReplaceAll(absent, "a-collection-nobody-ever-mounted", "<name>")
		if h != a {
			t.Errorf("a hidden collection must error identically to an absent one:\n hidden: %s\n absent: %s", h, a)
		}
	})

	t.Run("a qualified page id for a hidden collection 404s", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "secrets:payroll/salaries"})
		if !isErr {
			t.Fatalf("expected an error, got: %s", body)
		}
		if !strings.Contains(body, "not found") {
			t.Errorf("a qualified ID for a hidden collection should 404, not 403: %s", body)
		}
	})

	t.Run("a guessed bare id in a hidden collection 404s", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "payroll/salaries"})
		if !isErr {
			t.Fatalf("expected an error, got: %s", body)
		}
		if !strings.Contains(body, "not found") {
			t.Errorf("want a not-found error, got: %s", body)
		}
	})
}

// TestHostedAnonymous_AmbiguityCountsOnlyThePublishedSet publishes TWO
// collections, so the ambiguity error itself is exercised for an
// anonymous caller: it must count 2, never the mounted 3.
func TestHostedAnonymous_AmbiguityCountsOnlyThePublishedSet(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{
		{Name: "public", Anonymous: true, Collections: []string{"runbooks", "architecture"}},
	})
	ctx := context.Background()
	c := f.client(ctx, "")

	body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "shared/overview"})
	if !isErr {
		t.Fatalf("expected an ambiguity error, got: %s", body)
	}
	if !strings.Contains(body, "exists in 2 collections") {
		t.Errorf("ambiguity count must be the published view (2), not the mounted set (3): %s", body)
	}
	if strings.Contains(body, "secrets") {
		t.Errorf("the ambiguity error named an unpublished collection: %s", body)
	}
}

// --- enumeration -------------------------------------------------------

// TestHostedAnonymous_ListCollectionsShowsExactlyThePublishedSet is the
// acceptance criterion for mk_list_collections: the published set, with
// read and nothing but read.
func TestHostedAnonymous_ListCollectionsShowsExactlyThePublishedSet(t *testing.T) {
	f := newHostedFixture(t, publicRules())
	ctx := context.Background()
	c := f.client(ctx, "")

	body, isErr := callText(t, ctx, c, toolListCollections, map[string]any{})
	if isErr {
		t.Fatalf("mk_list_collections failed: %s", body)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d collections, want exactly the published 1: %s", len(entries), body)
	}
	if got := fmt.Sprint(entries[0]["name"]); got != "runbooks" {
		t.Errorf("name = %q, want runbooks", got)
	}
	raw, _ := entries[0]["capabilities"].([]any)
	caps := make([]string, len(raw))
	for i, c := range raw {
		caps[i] = fmt.Sprint(c)
	}
	if strings.Join(caps, ",") != "read" {
		t.Errorf("capabilities = %v, want exactly [read] — an anonymous rule is read-only by validation", caps)
	}
}

// TestHostedAnonymous_TwoCallersSeeDifferentServers is the acceptance
// criterion stated as one test: the same process, the same tools/list
// request, two different answers, decided entirely by whether an
// Authorization header was present.
func TestHostedAnonymous_TwoCallersSeeDifferentServers(t *testing.T) {
	f := newHostedFixture(t, publicRules())
	ctx := context.Background()

	anon := f.client(ctx, "")
	hr := f.client(ctx, f.token("bob", "hr"))

	descriptions := func(c *mcpclient.Client) string {
		res, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		blob, _ := json.Marshal(res.Tools)
		return string(blob)
	}

	anonTools, hrTools := descriptions(anon), descriptions(hr)
	if anonTools == hrTools {
		t.Fatal("two callers with different grants must get different tool descriptions")
	}
	if !strings.Contains(anonTools, "runbooks") || strings.Contains(anonTools, "secrets") {
		t.Errorf("the anonymous caller's tools should name runbooks and not secrets: %s", anonTools)
	}
	// The hr caller sees BOTH: their own rule, unioned with the published
	// collection. Publishing must not require restating it in their rule.
	if !strings.Contains(hrTools, "secrets") || !strings.Contains(hrTools, "runbooks") {
		t.Errorf("an authenticated caller should see their own AND the published collections: %s", hrTools)
	}

	// The same asymmetry through the enumeration surface.
	anonList, _ := callText(t, ctx, anon, toolListCollections, map[string]any{})
	hrList, _ := callText(t, ctx, hr, toolListCollections, map[string]any{})
	if strings.Contains(anonList, "secrets") {
		t.Errorf("anonymous enumeration leaked secrets: %s", anonList)
	}
	if !strings.Contains(hrList, "secrets") || !strings.Contains(hrList, "runbooks") {
		t.Errorf("authenticated enumeration should carry both: %s", hrList)
	}
}

// TestHostedAnonymous_PublishingDoesNotWidenAnAuthenticatedCaller checks
// the union is exactly a union: the hr caller gains the published
// collection and nothing else, and `architecture` — granted to nobody —
// stays invisible to both.
func TestHostedAnonymous_PublishingDoesNotWidenAnAuthenticatedCaller(t *testing.T) {
	f := newHostedFixture(t, publicRules())
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		token string
		want  []string
	}{
		{"anonymous", "", []string{"runbooks"}},
		{"hr", f.token("bob", "hr"), []string{"runbooks", "secrets"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := f.client(ctx, tc.token)
			body, isErr := callText(t, ctx, c, toolListCollections, map[string]any{})
			if isErr {
				t.Fatalf("mk_list_collections failed: %s", body)
			}
			var entries []map[string]any
			if err := json.Unmarshal([]byte(body), &entries); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, body)
			}
			seen := make([]string, len(entries))
			for i, e := range entries {
				seen[i] = fmt.Sprint(e["name"])
			}
			if strings.Join(seen, ",") != strings.Join(tc.want, ",") {
				t.Errorf("collections = %v, want %v", seen, tc.want)
			}
			if strings.Contains(body, "architecture") {
				t.Errorf("architecture is granted to nobody and must be invisible to everyone: %s", body)
			}
		})
	}
}

// --- memory ------------------------------------------------------------

// TestHostedAnonymous_OwnsNoMemoriesAndGetsNoWriteTool is #27's
// fail-closed rule, re-asserted for the caller #36 introduces. An
// anonymous caller's grants are read-only by validation, so the memory
// tool is filtered out; and even reached directly it refuses, because the
// namespace a personal memory would land in comes from a verified subject
// this caller does not have.
func TestHostedAnonymous_OwnsNoMemoriesAndGetsNoWriteTool(t *testing.T) {
	f := newMemFixture(t, []authz.Rule{
		{Name: "public", Anonymous: true, Collections: []string{"notes"}, Capabilities: []string{"read"}},
		{Name: "authors", Groups: []string{"authors"}, Collections: []string{"notes"},
			Capabilities: []string{"read", "personal-write"}},
	})
	ctx := context.Background()

	// An authenticated author saves a personal memory, so there is one to
	// leak.
	author := f.client(ctx, f.token("alice", "authors"))
	if _, text, isErr := callSaveMemory(t, ctx, author, map[string]any{
		"scope": "personal", "title": "Alice's note", "content": "an unmistakable phrase: pomegranate",
		"collection": "notes",
	}); isErr {
		t.Fatalf("author save failed: %s", text)
	}

	anon := f.client(ctx, "")

	t.Run("no write tool is offered", func(t *testing.T) {
		res, err := anon.ListTools(ctx, mcpapi.ListToolsRequest{})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		for _, tool := range res.Tools {
			if tool.Name == toolSaveMemory {
				t.Fatal("an anonymous caller must not be offered mk_save_memory")
			}
		}
	})

	t.Run("calling it anyway is refused by the filter", func(t *testing.T) {
		_, err := anon.CallTool(ctx, mcpapi.CallToolRequest{
			Params: mcpapi.CallToolParams{Name: toolSaveMemory, Arguments: map[string]any{
				"scope": "personal", "title": "t", "content": "c", "collection": "notes",
			}},
		})
		if err == nil {
			t.Fatal("a filtered-out tool must not be invocable")
		}
	})

	t.Run("another principal's personal memory is invisible", func(t *testing.T) {
		body, isErr := callText(t, ctx, anon, toolSearch, map[string]any{"query": "pomegranate"})
		if isErr {
			t.Fatalf("mk_search failed: %s", body)
		}
		if strings.Contains(body, "pomegranate") || strings.Contains(body, "Alice's note") {
			t.Errorf("an anonymous caller read another principal's personal memory: %s", body)
		}
		list, isErr := callText(t, ctx, anon, toolList, map[string]any{})
		if isErr {
			t.Fatalf("mk_list failed: %s", list)
		}
		if strings.Contains(list, "memory/personal") {
			t.Errorf("mk_list exposed a personal memory to an anonymous caller: %s", list)
		}
	})
}

// TestHostedAnonymous_AdmissionIsGenuineButCarriesNoWriteTool separates
// the two halves of the previous test's claim: the anonymous caller IS
// admitted (the read tools are there, so the absence of the memory tool
// is not just a blanket refusal), and the refusal is fail-closed one
// layer below the tool filter too.
//
// The second half matters because the tool filter is a display-and-access
// boundary that a later transport could wire differently, while
// memory.Authorize is the thing that decides whose namespace a write
// lands in. It is asked here with the EXACT grants the policy synthesizes
// for an anonymous caller, rather than a hand-built approximation.
func TestHostedAnonymous_AdmissionIsGenuineButCarriesNoWriteTool(t *testing.T) {
	rules := []authz.Rule{{Name: "public", Anonymous: true, Collections: []string{"notes"}}}
	f := newMemFixture(t, rules)
	ctx := context.Background()
	c := f.client(ctx, "")

	res, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("the anonymous caller should hold the published collection's read tools")
	}
	for _, tool := range res.Tools {
		if tool.Name == toolSaveMemory {
			t.Error("mk_save_memory must not be among them")
		}
	}

	policy, err := authz.NewPolicy(&authz.Config{
		Resource:  testResource,
		Providers: []authz.Provider{{Issuer: f.issuer.URL, Audience: testAudience}},
		Rules:     rules,
	})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	grants := policy.EvaluateAnonymous()
	for _, scope := range memory.AllScopes() {
		// false: the hosted transport never sets AllowAnonymousPersonal.
		if _, aerr := memory.Authorize(grants, "notes", scope, false); aerr == nil {
			t.Errorf("memory.Authorize permitted a %s write for the anonymous grants", scope)
		}
	}
}

// --- telemetry, logs, and zero-behaviour-change -------------------------

func TestHostedAnonymous_OutcomeIsClassifiedInMetricsAndLogs(t *testing.T) {
	f := newHostedFixture(t, publicRules())
	ctx := context.Background()

	c := f.client(ctx, "")
	if body, isErr := callText(t, ctx, c, toolList, map[string]any{}); isErr {
		t.Fatalf("mk_list failed: %s", body)
	}

	resp := f.do(t, http.MethodGet, MetricsPath, "")
	defer resp.Body.Close()
	metrics := readAll(t, resp)

	if !strings.Contains(metrics, "meerkat_auth_anonymous_total") {
		t.Errorf("no anonymous-admission counter in /metrics:\n%s", metrics)
	}
	// It is an admission, not a failure.
	if strings.Contains(metrics, `meerkat_auth_failures_total{reason="anonymous"}`) {
		t.Error("an admitted request was counted as an auth failure")
	}
	// And no label anywhere names what was published.
	for _, forbidden := range []string{"runbooks", "architecture", "secrets"} {
		if strings.Contains(metrics, forbidden) {
			t.Errorf("metrics leaked the published/mounted set (%q):\n%s", forbidden, metrics)
		}
	}

	logs := f.logs.String()
	if !strings.Contains(logs, `"auth":"anonymous"`) {
		t.Errorf("the access log should classify the anonymous outcome:\n%s", logs)
	}
	// It must NOT invent identity fields for a caller who has none.
	for _, invented := range []string{`"sub":""`, `"issuer":""`, `"tenant":""`} {
		if strings.Contains(logs, invented) {
			t.Errorf("the access log invented an identity field %s for an anonymous caller:\n%s", invented, logs)
		}
	}
}

// TestHostedAnonymous_NoAnonymousRuleChangesNothing is the
// zero-behaviour-change acceptance criterion: the identical assertions
// the pre-#36 tests make, re-run to prove they still hold for a policy
// that writes no anonymous rule.
func TestHostedAnonymous_NoAnonymousRuleChangesNothing(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{
		{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}},
		{Name: "everyone-authenticated", Collections: []string{"architecture"}},
	})

	if f.srv.AnonymousEnabled() {
		t.Error("AnonymousEnabled() must be false for a policy with no anonymous rule")
	}

	t.Run("no token is still 401", func(t *testing.T) {
		resp := f.do(t, http.MethodPost, f.srv.EndpointPath(), "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata=") {
			t.Errorf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
		}
	})

	t.Run("the selector-less rule still means every AUTHENTICATED caller", func(t *testing.T) {
		// This is the trap #36 had to avoid: a rule with no selector must
		// not have quietly become a rule that also matches nobody-at-all.
		ctx := context.Background()
		c := f.client(ctx, f.token("alice", "sre"))
		body, isErr := callText(t, ctx, c, toolListCollections, map[string]any{})
		if isErr {
			t.Fatalf("mk_list_collections failed: %s", body)
		}
		if !strings.Contains(body, "runbooks") || !strings.Contains(body, "architecture") {
			t.Errorf("an authenticated caller should hold both rules' collections: %s", body)
		}
	})

	t.Run("the anonymous counter stays at zero and the log gains no field", func(t *testing.T) {
		resp := f.do(t, http.MethodGet, MetricsPath, "")
		defer resp.Body.Close()
		body := readAll(t, resp)
		if !strings.Contains(body, "meerkat_auth_anonymous_total 0") {
			t.Errorf("want the anonymous counter present and zero:\n%s", body)
		}
		if strings.Contains(f.logs.String(), `"auth":"anonymous"`) {
			t.Error("no request was admitted anonymously, so no line should say so")
		}
	})
}

// TestHostedAnonymous_RootBannerSaysSo: `/` is unauthenticated, so it
// states the POSTURE and names nothing. An operator (or an auditor)
// hitting the root of a published server should not have to read the
// config to learn that it answers token-less requests.
func TestHostedAnonymous_RootBannerSaysSo(t *testing.T) {
	published := newHostedFixture(t, publicRules())
	resp := published.do(t, http.MethodGet, "/", "")
	body := readAll(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "published to unauthenticated callers") {
		t.Errorf("the banner should state the posture: %s", body)
	}
	for _, name := range []string{"runbooks", "architecture", "secrets"} {
		if strings.Contains(body, name) {
			t.Errorf("the unauthenticated banner named a collection (%q): %s", name, body)
		}
	}

	private := newHostedFixture(t, []authz.Rule{{Groups: []string{"sre"}, Collections: []string{"runbooks"}}})
	resp = private.do(t, http.MethodGet, "/", "")
	body = readAll(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "published to unauthenticated callers") {
		t.Errorf("a policy with no anonymous rule must not claim otherwise: %s", body)
	}
	if !strings.Contains(body, "OIDC bearer token required") {
		t.Errorf("banner = %s", body)
	}
}

// --- fixture helpers ----------------------------------------------------

// testClaims is the standard claim set for a test caller — the same one
// hostedFixture.token mints, factored out so a test can vary one field.
func testClaims(subject string, groups ...string) authntest.Claims {
	return authntest.Claims{
		Subject: subject, Audience: testAudience, Groups: groups,
		Email: subject + "@example.com", Tenant: "acme",
	}
}

// expiredToken mints a real, correctly-signed token that expired an hour
// ago. It is the "present but invalid" case that must never become
// anonymous access, and it is a REAL expiry checked by go-oidc rather
// than a stub, so the test exercises the verification path a production
// token takes.
func (f *hostedFixture) expiredToken(subject string) string {
	f.t.Helper()
	c := testClaims(subject)
	c.IssuedAt = time.Now().Add(-2 * time.Hour)
	c.Expiry = time.Now().Add(-time.Hour)
	return f.issuer.Token(f.t, c)
}
