package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpapi "github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// hosted_test.go covers the Streamable HTTP transport end to end: the
// 401/WWW-Authenticate contract, RFC 9728 metadata, the probes, the
// metrics, concurrent sessions, and — the part that matters most — that
// a collection a caller may not read is invisible through every MCP
// surface rather than merely denied.

const (
	testAudience = "api://meerkat"
	testResource = "https://mcp.example.com/mcp"
)

// hostedFixture is a running hosted server plus the fake issuer whose
// tokens it accepts.
type hostedFixture struct {
	t      *testing.T
	srv    *HostedServer
	http   *httptest.Server
	issuer *authntest.Issuer
	logs   *bytes.Buffer
}

// threeCollectionRegistry mounts runbooks, architecture and secrets.
// "shared/overview" is in all three, so an ambiguity error that counted
// hidden collections would be visible immediately. "payroll/salaries"
// and the word "compensation" exist only in secrets.
func threeCollectionRegistry(t *testing.T) *collections.Registry {
	t.Helper()
	reg, err := collections.New(
		collections.FromPages("runbooks", []kb.Page{
			testPage("incidents/paging", "Paging", "who to page during an incident", "runbooks", "reviewed", "team-a"),
			testPage("shared/overview", "Runbook Overview", "shared overview text", "runbooks", "reviewed", "team-a"),
		}),
		collections.FromPages("architecture", []kb.Page{
			testPage("adr/0001", "ADR 1", "we chose object storage", "adr", "reviewed", "team-b"),
			testPage("shared/overview", "Architecture Overview", "shared overview text", "adr", "reviewed", "team-b"),
		}),
		collections.FromPages("secrets", []kb.Page{
			testPage("payroll/salaries", "Salaries", "confidential compensation data", "hr", "reviewed", "team-c"),
			testPage("shared/overview", "Secrets Overview", "shared overview text", "hr", "reviewed", "team-c"),
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return reg
}

// newHostedFixture starts a hosted server over threeCollectionRegistry
// with the given policy rules. Passing no rules at all leaves the server
// unauthenticated.
func newHostedFixture(t *testing.T, rules []authz.Rule) *hostedFixture {
	t.Helper()
	f := &hostedFixture{t: t, logs: &bytes.Buffer{}}

	var authCfg *authz.Config
	var client *http.Client
	if rules != nil {
		f.issuer = authntest.NewIssuer(t)
		client = f.issuer.Client()
		authCfg = &authz.Config{
			Resource:  testResource,
			Providers: []authz.Provider{{Issuer: f.issuer.URL, Audience: testAudience}},
			Rules:     rules,
		}
	}

	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: threeCollectionRegistry(t),
		Auth:        authCfg,
		Version:     "test",
		HTTPClient:  client,
		Logger:      slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	f.srv = srv
	f.http = httptest.NewServer(srv.Handler())
	t.Cleanup(f.http.Close)
	return f
}

// token mints a bearer token for a caller in the given groups.
func (f *hostedFixture) token(subject string, groups ...string) string {
	f.t.Helper()
	return f.issuer.Token(f.t, authntest.Claims{
		Subject: subject, Audience: testAudience, Groups: groups,
		Email: subject + "@example.com", Tenant: "acme",
	})
}

// mcpURL is the MCP endpoint of the running test server.
func (f *hostedFixture) mcpURL() string { return f.http.URL + f.srv.EndpointPath() }

// client dials the MCP endpoint with the given bearer token (empty for
// none) and completes the initialize handshake.
func (f *hostedFixture) client(ctx context.Context, bearer string) *mcpclient.Client {
	f.t.Helper()
	var opts []transport.StreamableHTTPCOption
	if bearer != "" {
		opts = append(opts, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + bearer}))
	}
	c, err := mcpclient.NewStreamableHttpClient(f.mcpURL(), opts...)
	if err != nil {
		f.t.Fatalf("new client: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		f.t.Fatalf("start client: %v", err)
	}
	f.t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Initialize(ctx, mcpapi.InitializeRequest{}); err != nil {
		f.t.Fatalf("initialize: %v", err)
	}
	return c
}

// callText invokes a tool and returns its text content.
func callText(t *testing.T, ctx context.Context, c *mcpclient.Client, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := c.CallTool(ctx, mcpapi.CallToolRequest{
		Params: mcpapi.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var sb strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(mcpapi.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// --- authentication ---------------------------------------------------

func TestHosted_UnauthenticatedRequestIs401WithChallenge(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Collections: []string{"runbooks"}}})

	req, err := http.NewRequest(http.MethodPost, f.mcpURL(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q", challenge)
	}
	want := "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"
	if !strings.Contains(challenge, `resource_metadata="`+want+`"`) {
		t.Errorf("WWW-Authenticate = %q, want resource_metadata pointing at %s", challenge, want)
	}
}

func TestHosted_ForgedTokenIs401(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Collections: []string{"runbooks"}}})
	forged := f.issuer.TokenSignedByOther(t, authntest.Claims{Subject: "mallory", Audience: testAudience})

	resp := f.do(t, http.MethodPost, f.srv.EndpointPath(), forged)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestHosted_TokenWithNoMatchingRuleIs403(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}}})

	resp := f.do(t, http.MethodPost, f.srv.EndpointPath(), f.token("outsider", "interns"))
	defer resp.Body.Close()
	body := readAll(t, resp)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	for _, name := range []string{"runbooks", "architecture", "secrets"} {
		if strings.Contains(body, name) {
			t.Errorf("a 403 must not name a collection (%q leaked): %s", name, body)
		}
	}
}

func TestHosted_MetadataEndpointIsPublicAndServedAtBothPaths(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Collections: []string{"runbooks"}}})

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		t.Run(path, func(t *testing.T) {
			resp := f.do(t, http.MethodGet, path, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 without a token", resp.StatusCode)
			}
			var doc struct {
				Resource             string   `json:"resource"`
				AuthorizationServers []string `json:"authorization_servers"`
			}
			if err := json.Unmarshal([]byte(readAll(t, resp)), &doc); err != nil {
				t.Fatalf("body: %v", err)
			}
			if doc.Resource != testResource {
				t.Errorf("resource = %q", doc.Resource)
			}
			if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != f.issuer.URL {
				t.Errorf("authorization_servers = %v", doc.AuthorizationServers)
			}
		})
	}
}

func TestHosted_NoAuthConfiguredNeedsNoToken(t *testing.T) {
	f := newHostedFixture(t, nil)
	if f.srv.AuthEnabled() {
		t.Fatal("AuthEnabled() should be false with no providers")
	}

	ctx := context.Background()
	c := f.client(ctx, "")
	body, isErr := callText(t, ctx, c, toolList, map[string]any{})
	if isErr {
		t.Fatalf("mk_list failed: %s", body)
	}
	for _, name := range []string{"runbooks", "architecture", "secrets"} {
		if !strings.Contains(body, `"collection": "`+name+`"`) {
			t.Errorf("with no auth configured every collection should be listed; %q missing", name)
		}
	}
}

// --- invisibility ------------------------------------------------------

// TestHosted_HiddenCollectionIsInvisibleEverywhere is the acceptance
// test for the design's central rule. Each subtest is an enumeration
// path a merely-denying implementation would leak through.
func TestHosted_HiddenCollectionIsInvisibleEverywhere(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{
		{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks", "architecture"}},
		{Name: "hr", Groups: []string{"hr"}, Collections: []string{"secrets"}},
	})
	ctx := context.Background()
	c := f.client(ctx, f.token("alice", "sre"))

	t.Run("tool descriptions", func(t *testing.T) {
		res, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		if len(res.Tools) != 4 {
			t.Fatalf("got %d tools, want 4", len(res.Tools))
		}
		for _, tool := range res.Tools {
			blob, _ := json.Marshal(tool)
			text := string(blob)
			if strings.Contains(text, "secrets") {
				t.Errorf("tool %q names a hidden collection: %s", tool.Name, text)
			}
			if !strings.Contains(text, "runbooks") || !strings.Contains(text, "architecture") {
				t.Errorf("tool %q should name the caller's own collections: %s", tool.Name, text)
			}
			// "2 collections", never 3.
			if strings.Contains(text, "mounts 3 collections") {
				t.Errorf("tool %q reports the mounted count rather than the visible one", tool.Name)
			}
		}
	})

	t.Run("list", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolList, map[string]any{})
		if isErr {
			t.Fatalf("mk_list failed: %s", body)
		}
		if strings.Contains(body, "secrets") || strings.Contains(body, "payroll/salaries") {
			t.Errorf("mk_list leaked the hidden collection: %s", body)
		}
		if !strings.Contains(body, "incidents/paging") {
			t.Errorf("mk_list should return the caller's pages: %s", body)
		}
	})

	t.Run("list collections", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolListCollections, map[string]any{})
		if isErr {
			t.Fatalf("mk_list_collections failed: %s", body)
		}
		if strings.Contains(body, "secrets") {
			t.Errorf("mk_list_collections leaked the hidden collection: %s", body)
		}
		var entries []map[string]any
		if err := json.Unmarshal([]byte(body), &entries); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, body)
		}
		if len(entries) != 2 {
			t.Fatalf("got %d collections, want exactly the caller's 2 (runbooks, architecture): %s", len(entries), body)
		}
		names := map[string]bool{}
		for _, e := range entries {
			names[fmt.Sprint(e["name"])] = true
		}
		if !names["runbooks"] || !names["architecture"] {
			t.Errorf("mk_list_collections should name the caller's own collections, got %v", names)
		}
	})

	t.Run("search", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolSearch, map[string]any{"query": "compensation"})
		if isErr {
			t.Fatalf("mk_search failed: %s", body)
		}
		if strings.Contains(body, "payroll") || strings.Contains(body, "secrets") {
			t.Errorf("mk_search leaked a hidden hit: %s", body)
		}
	})

	t.Run("show ambiguity counts only visible collections", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "shared/overview"})
		if !isErr {
			t.Fatalf("expected an ambiguity error, got: %s", body)
		}
		if !strings.Contains(body, "exists in 2 collections") {
			t.Errorf("ambiguity count must be the caller's view (2), not the mounted set (3): %s", body)
		}
		if strings.Contains(body, "secrets") {
			t.Errorf("the ambiguity error named a hidden collection: %s", body)
		}
	})

	t.Run("show into a hidden collection", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{
			"id": "payroll/salaries", "collection": "secrets",
		})
		if !isErr {
			t.Fatalf("expected an error, got: %s", body)
		}
		if !strings.Contains(body, "unknown collection") {
			t.Errorf("naming a hidden collection should read as unknown: %s", body)
		}
		if strings.Contains(body, "available: runbooks, architecture, secrets") {
			t.Errorf("the available: list leaked the mounted set: %s", body)
		}
	})

	t.Run("qualified page id for a hidden collection", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "secrets:payroll/salaries"})
		if !isErr {
			t.Fatalf("expected an error, got: %s", body)
		}
		if !strings.Contains(body, "not found") {
			t.Errorf("a qualified ID for a hidden collection should 404, not 403: %s", body)
		}
	})

	t.Run("a bare id only in the hidden collection", func(t *testing.T) {
		body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "payroll/salaries"})
		if !isErr {
			t.Fatalf("expected an error, got: %s", body)
		}
		if !strings.Contains(body, "not found") {
			t.Errorf("want a not-found error, got: %s", body)
		}
	})
}

// TestHostedListCollections_CapabilitiesReflectTheCallersGrants proves
// mk_list_collections' "capabilities" field is the CALLER's own
// effective grants (authz.Grants.Capabilities), not a fixed or
// server-wide value: two callers with different rules over the same
// mounted collections must see different capability lists.
func TestHostedListCollections_CapabilitiesReflectTheCallersGrants(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{
		{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}, Capabilities: []string{"read"}},
		{Name: "leads", Groups: []string{"leads"}, Collections: []string{"runbooks", "architecture"},
			Capabilities: []string{"read", "personal-write"}},
	})
	ctx := context.Background()

	cases := []struct {
		group string
		want  map[string][]string // collection name -> expected capabilities
	}{
		{"sre", map[string][]string{"runbooks": {"read"}}},
		{"leads", map[string][]string{
			"runbooks":     {"read", "personal-write"},
			"architecture": {"read", "personal-write"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.group, func(t *testing.T) {
			c := f.client(ctx, f.token(tc.group+"-caller", tc.group))
			body, isErr := callText(t, ctx, c, toolListCollections, map[string]any{})
			if isErr {
				t.Fatalf("mk_list_collections failed: %s", body)
			}
			var entries []map[string]any
			if err := json.Unmarshal([]byte(body), &entries); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, body)
			}
			seen := map[string][]string{}
			for _, e := range entries {
				name := fmt.Sprint(e["name"])
				raw, _ := e["capabilities"].([]any)
				caps := make([]string, len(raw))
				for i, c := range raw {
					caps[i] = fmt.Sprint(c)
				}
				seen[name] = caps
			}
			if len(seen) != len(tc.want) {
				t.Fatalf("%s: got collections %v, want exactly %v", tc.group, seen, tc.want)
			}
			for name, want := range tc.want {
				got := seen[name]
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s: capabilities for %q = %v, want %v", tc.group, name, got, want)
				}
			}
		})
	}
}

func TestHosted_SingleVisibleCollectionGetsSingleCollectionUX(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{
		{Name: "hr", Groups: []string{"hr"}, Collections: []string{"secrets"}},
	})
	ctx := context.Background()
	c := f.client(ctx, f.token("bob", "hr"))

	res, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		blob, _ := json.Marshal(tool)
		if !strings.Contains(string(blob), "mounts a single collection (secrets)") {
			t.Errorf("tool %q should present as single-collection: %s", tool.Name, blob)
		}
	}
	// And "shared/overview" resolves without ambiguity.
	body, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "shared/overview"})
	if isErr {
		t.Fatalf("mk_show failed: %s", body)
	}
	if !strings.Contains(body, "Secrets Overview") {
		t.Errorf("expected the secrets copy of the page: %s", body)
	}
}

func TestHosted_GrantForAnUnmountedCollectionYieldsNoTools(t *testing.T) {
	// The caller is granted a collection this deployment doesn't mount.
	// They authenticate (so the gate lets them through) but have nothing
	// to read, and must not be offered tools that can only fail.
	f := newHostedFixture(t, []authz.Rule{
		{Name: "ghost", Collections: []string{"a-collection-that-was-decommissioned"}},
	})
	ctx := context.Background()
	c := f.client(ctx, f.token("carol"))

	res, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("got %d tools, want none for a caller with no readable collection", len(res.Tools))
	}
	// And calling one anyway is refused by the same filter — mcp-go
	// applies tool filters to tools/call, so this is an access boundary
	// and not just a display fix.
	_, err = c.CallTool(ctx, mcpapi.CallToolRequest{
		Params: mcpapi.CallToolParams{Name: toolList, Arguments: map[string]any{}},
	})
	if err == nil {
		t.Fatal("a filtered-out tool must not be invocable")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("the refusal should read as 'tool not found', got %v", err)
	}
}

func TestHosted_RejectsAnEndpointPathThatCollidesWithAnUnauthenticatedRoute(t *testing.T) {
	for _, path := range []string{LivenessPath, ReadinessPath, MetricsPath, "/.well-known/oauth-protected-resource"} {
		_, err := NewHosted(context.Background(), HostedConfig{
			Collections:  threeCollectionRegistry(t),
			EndpointPath: path,
			Logger:       slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		})
		if err == nil {
			t.Errorf("--path %s should be refused: it is outside the authentication gate", path)
		}
	}
	if _, err := NewHosted(context.Background(), HostedConfig{
		Collections:  threeCollectionRegistry(t),
		EndpointPath: "mcp",
		Logger:       slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}); err == nil {
		t.Error("a path without a leading slash should be refused")
	}
}

// --- probes -------------------------------------------------------------

func TestHosted_ProbesAndMetricsAreReachableWithoutAToken(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Collections: []string{"runbooks"}}})

	for _, path := range []string{LivenessPath, ReadinessPath, MetricsPath} {
		resp := f.do(t, http.MethodGet, path, "")
		body := readAll(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 without a token", path, resp.StatusCode)
		}
		// No probe may enumerate the mounted set.
		for _, name := range []string{"runbooks", "architecture", "secrets"} {
			if strings.Contains(body, name) {
				t.Errorf("GET %s leaked collection name %q: %s", path, name, body)
			}
		}
	}
}

func TestHosted_ReadyzReflectsContentHealth(t *testing.T) {
	dirs := map[string]string{"alpha": t.TempDir(), "beta": t.TempDir()}
	var resolved []contentsource.ResolvedCollection
	for _, name := range []string{"alpha", "beta"} {
		dir := dirs[name]
		writeFixturePage(t, dir, "index.md", "# "+name+"\n\nbody text\n")
		resolved = append(resolved, contentsource.ResolvedCollection{
			Name: name, Dir: dir, Provenance: "disk:" + dir,
			Source: contentsource.Source{Type: contentsource.TypeLocal, Layout: contentsource.MergeLayout(contentsource.Layout{})},
		})
	}
	reg, err := collections.Open(context.Background(), resolved)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections:  reg,
		Version:      "test",
		Logger:       slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		ReadinessTTL: time.Nanosecond, // no caching, so the transition is observable
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func() (int, string) {
		t.Helper()
		resp, gerr := ts.Client().Get(ts.URL + ReadinessPath)
		if gerr != nil {
			t.Fatal(gerr)
		}
		defer resp.Body.Close()
		return resp.StatusCode, readAll(t, resp)
	}

	code, body := get()
	if code != http.StatusOK {
		t.Fatalf("readyz = %d (%s), want 200 while both collections serve", code, body)
	}
	if !strings.Contains(body, `"ready":2`) || !strings.Contains(body, `"total":2`) {
		t.Errorf("readyz body = %s, want ready=2 total=2", body)
	}

	// Pull one collection's content out from under the running server —
	// an unmounted volume, a vanished bind mount. Readiness must notice.
	if err := os.RemoveAll(dirs["beta"]); err != nil {
		t.Fatal(err)
	}
	code, body = get()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d (%s), want 503 once a collection stops serving", code, body)
	}
	if !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, `"ready":1`) {
		t.Errorf("readyz body = %s, want degraded with ready=1", body)
	}

	// Liveness is unaffected: restarting the process would not remount
	// the volume, so a liveness failure would only produce a crash loop.
	resp, err := ts.Client().Get(ts.URL + LivenessPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("livez = %d, want 200 — content health is a readiness concern", resp.StatusCode)
	}
}

// --- metrics and logging -------------------------------------------------

func TestHosted_MetricsRecordTrafficAndAuthFailures(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}}})
	ctx := context.Background()

	// One refused request and one successful tool call.
	f.do(t, http.MethodPost, f.srv.EndpointPath(), "").Body.Close()
	c := f.client(ctx, f.token("alice", "sre"))
	if body, isErr := callText(t, ctx, c, toolList, map[string]any{}); isErr {
		t.Fatalf("mk_list failed: %s", body)
	}

	resp := f.do(t, http.MethodGet, MetricsPath, "")
	defer resp.Body.Close()
	body := readAll(t, resp)

	for _, want := range []string{
		`meerkat_auth_failures_total{reason="missing_token"} 1`,
		`meerkat_mcp_tool_calls_total{outcome="ok",tool="mk_list"} 1`,
		`meerkat_collections_mounted 3`,
		`meerkat_build_info{version="test"} 1`,
		`meerkat_http_requests_total`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// Cardinality discipline: no metric may carry a page ID, a query or
	// a caller subject as a label.
	for _, forbidden := range []string{"incidents/paging", "alice", "secrets"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("metrics leaked %q as a label", forbidden)
		}
	}
}

func TestHosted_UnmatchedPathsCollapseToOneMetricLabel(t *testing.T) {
	f := newHostedFixture(t, nil)
	for _, p := range []string{"/wp-admin", "/.env", "/phpmyadmin"} {
		f.do(t, http.MethodGet, p, "").Body.Close()
	}
	resp := f.do(t, http.MethodGet, MetricsPath, "")
	defer resp.Body.Close()
	body := readAll(t, resp)

	if !strings.Contains(body, `route="other"`) {
		t.Error(`unmatched paths should be labelled route="other"`)
	}
	for _, p := range []string{"wp-admin", ".env", "phpmyadmin"} {
		if strings.Contains(body, p) {
			t.Errorf("a scanned path became a metric label: %q", p)
		}
	}
}

func TestHosted_AccessLogRecordsIdentityButNeverTheToken(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}}})
	tok := f.token("alice", "sre")
	ctx := context.Background()
	c := f.client(ctx, tok)
	if body, isErr := callText(t, ctx, c, toolList, map[string]any{}); isErr {
		t.Fatalf("mk_list failed: %s", body)
	}

	logs := f.logs.String()
	if !strings.Contains(logs, `"msg":"mcp.access"`) {
		t.Fatalf("no access log line emitted:\n%s", logs)
	}
	if !strings.Contains(logs, `"sub":"alice"`) {
		t.Errorf("access log should record the caller's subject:\n%s", logs)
	}
	if !strings.Contains(logs, `"tenant":"acme"`) {
		t.Errorf("access log should record the tenant:\n%s", logs)
	}
	if strings.Contains(logs, tok) {
		t.Error("the access log carried the bearer token")
	}
	// Group membership is deliberately not logged.
	if strings.Contains(logs, `"sre"`) {
		t.Errorf("the access log carried group membership:\n%s", logs)
	}
}

func TestHosted_DeniedRequestsAreLoggedWithAReason(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{{Groups: []string{"sre"}, Collections: []string{"runbooks"}}})
	f.do(t, http.MethodPost, f.srv.EndpointPath(), "").Body.Close()

	logs := f.logs.String()
	if !strings.Contains(logs, `"msg":"mcp.auth_denied"`) || !strings.Contains(logs, `"reason":"missing_token"`) {
		t.Errorf("expected a denial log line with a reason:\n%s", logs)
	}
}

// --- concurrency ---------------------------------------------------------

func TestHosted_ConcurrentSessions(t *testing.T) {
	f := newHostedFixture(t, []authz.Rule{
		{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}},
		{Name: "hr", Groups: []string{"hr"}, Collections: []string{"secrets"}},
	})

	// Two populations with disjoint access, interleaved, so a session
	// that bled another's grants would show up as a wrong answer rather
	// than only as a race.
	type caller struct {
		group   string
		wantHit string
		noHit   string
	}
	callers := []caller{
		{group: "sre", wantHit: "incidents/paging", noHit: "payroll/salaries"},
		{group: "hr", wantHit: "payroll/salaries", noHit: "incidents/paging"},
	}

	const perGroup = 6
	var wg sync.WaitGroup
	errCh := make(chan error, len(callers)*perGroup)

	for _, cl := range callers {
		for i := range perGroup {
			wg.Add(1)
			go func(cl caller, i int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				c := f.client(ctx, f.token(fmt.Sprintf("%s-%d", cl.group, i), cl.group))
				for range 3 {
					res, err := c.CallTool(ctx, mcpapi.CallToolRequest{
						Params: mcpapi.CallToolParams{Name: toolList, Arguments: map[string]any{}},
					})
					if err != nil {
						errCh <- fmt.Errorf("%s-%d: call: %w", cl.group, i, err)
						return
					}
					var sb strings.Builder
					for _, content := range res.Content {
						if tc, ok := content.(mcpapi.TextContent); ok {
							sb.WriteString(tc.Text)
						}
					}
					body := sb.String()
					if !strings.Contains(body, cl.wantHit) {
						errCh <- fmt.Errorf("%s-%d: missing %q in %s", cl.group, i, cl.wantHit, body)
						return
					}
					if strings.Contains(body, cl.noHit) {
						errCh <- fmt.Errorf("%s-%d: saw another caller's page %q", cl.group, i, cl.noHit)
						return
					}
				}
			}(cl, i)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// --- helpers --------------------------------------------------------------

// do issues a bare HTTP request against the test server. An empty
// bearer sends no Authorization header.
func (f *hostedFixture) do(t *testing.T, method, path, bearer string) *http.Response {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, f.http.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// writeFixturePage writes a markdown page into a content-repo-layout
// directory (wiki/ under the root).
func writeFixturePage(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, "wiki", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
