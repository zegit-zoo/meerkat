package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpapi "github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// memory_test.go drives mk_save_memory end to end over the hosted
// transport, against a real fake OIDC issuer and a real local memory
// store. The four properties it exists to pin:
//
//   - a caller cannot write into anybody else's namespace, whatever
//     they put in the arguments;
//   - a collection they cannot reach is invisible on the write path,
//     exactly as it is on every read path;
//   - a saved memory is searchable and showable immediately;
//   - two writers to one key cannot silently lose a memory.

// memFixture is a hosted server over two collections: "notes" (with a
// memory store) and "secrets" (with one too, so a hidden-collection
// test is about authorization rather than about configuration).
type memFixture struct {
	*hostedFixture
	notesDir   string
	secretsDir string
}

func newMemFixture(t *testing.T, rules []authz.Rule) *memFixture {
	t.Helper()
	f := &memFixture{hostedFixture: &hostedFixture{t: t, logs: &bytes.Buffer{}}}

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

	root := t.TempDir()
	f.notesDir = filepath.Join(root, "notes-memory")
	f.secretsDir = filepath.Join(root, "secrets-memory")

	reg, err := collections.New(
		withMemoryStore(t, collections.FromPages("notes", []kb.Page{
			testPage("handbook/onboarding", "Onboarding", "how we onboard", "handbook", "reviewed", "team-a"),
		}), f.notesDir),
		withMemoryStore(t, collections.FromPages("secrets", []kb.Page{
			testPage("payroll/salaries", "Salaries", "confidential compensation data", "hr", "reviewed", "team-c"),
		}), f.secretsDir),
	)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}

	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: reg,
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

// withMemoryStore attaches a fresh local memory store at dir.
func withMemoryStore(t *testing.T, c *collections.Collection, dir string) *collections.Collection {
	t.Helper()
	store, err := memory.OpenLocal(dir)
	if err != nil {
		t.Fatalf("OpenLocal(%s): %v", dir, err)
	}
	if err := c.AttachMemory(context.Background(), store); err != nil {
		t.Fatalf("AttachMemory: %v", err)
	}
	return c
}

// saveMemory calls mk_save_memory and returns the decoded result.
func callSaveMemory(t *testing.T, ctx context.Context, c *mcpclient.Client, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	text, isErr := callText(t, ctx, c, toolSaveMemory, args)
	if isErr {
		return nil, text, true
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("mk_save_memory result is not JSON: %v\n%s", err, text)
	}
	return out, text, false
}

// --- spoofing --------------------------------------------------------

func TestHostedMemory_NamespaceCannotBeSpoofed(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write"},
	}})

	victim := f.client(ctx, f.token("victim", "writers"))
	attacker := f.client(ctx, f.token("attacker", "writers"))

	// The victim saves a personal memory.
	got, _, isErr := callSaveMemory(t, ctx, victim, map[string]any{
		"scope": "personal", "key": "salary", "title": "Salary", "content": "the victim's private note",
	})
	if isErr {
		t.Fatalf("victim save failed: %v", got)
	}
	victimID, _ := got["id"].(string)
	victimNS, _ := got["namespace"].(string)
	if victimNS == "" || !strings.Contains(victimID, victimNS) {
		t.Fatalf("victim id/namespace = %q/%q", victimID, victimNS)
	}

	// Now the attacker tries every argument that could plausibly steer a
	// write into somebody else's space.
	for _, key := range []string{
		"../" + victimNS + "/salary",
		"../../" + victimNS + "/salary",
		"/" + victimNS + "/salary",
		victimNS + "/../" + victimNS + "/salary",
		"..%2f" + victimNS + "%2fsalary",
	} {
		got, text, isErr := callSaveMemory(t, ctx, attacker, map[string]any{
			"scope": "personal", "key": key, "title": "Overwrite", "content": "the attacker's content",
			// Several of these keys slugify to the same leaf name, so
			// later attempts would legitimately collide with the
			// attacker's own earlier one; replace turns that into an
			// update of their own document, which is exactly what should
			// happen and keeps the test about namespaces.
			"replace": true,
			// Deliberately also try to name the victim as the author: no
			// such argument exists, so these are simply ignored, which is
			// the property being asserted.
			"namespace": victimNS, "subject": "victim", "owner": "victim", "author": "victim",
		})
		if isErr {
			t.Fatalf("attacker save with key %q errored unexpectedly: %s", key, text)
		}
		gotNS, _ := got["namespace"].(string)
		gotID, _ := got["id"].(string)
		if gotNS == victimNS {
			t.Fatalf("attacker with key %q wrote into the victim's namespace %q", key, gotNS)
		}
		if gotID == victimID {
			t.Fatalf("attacker with key %q overwrote the victim's memory %q", key, gotID)
		}
		if !strings.HasPrefix(gotID, "memory/personal/"+gotNS+"/") {
			t.Errorf("attacker id = %q, want it under memory/personal/%s/", gotID, gotNS)
		}
	}

	// The victim's memory is byte-for-byte what they wrote.
	text, isErr := callText(t, ctx, victim, toolShow, map[string]any{"id": victimID, "collection": "notes"})
	if isErr {
		t.Fatalf("victim's memory is gone: %s", text)
	}
	if !strings.Contains(text, "the victim's private note") {
		t.Errorf("the victim's memory was modified: %s", text)
	}
	if strings.Contains(text, "attacker's content") {
		t.Errorf("the attacker's content landed in the victim's memory: %s", text)
	}
}

func TestHostedMemory_TwoIdentitiesGetDifferentNamespacesForTheSameKey(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write"},
	}})

	ids := make([]string, 0, 2)
	for _, sub := range []string{"alice", "bob"} {
		c := f.client(ctx, f.token(sub, "writers"))
		got, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
			"scope": "personal", "key": "preferences", "title": "Preferences", "content": "likes " + sub,
		})
		if isErr {
			t.Fatalf("%s save: %s", sub, text)
		}
		id, _ := got["id"].(string)
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Fatalf("two identities saving the same key collided on %q", ids[0])
	}
	// Neither is a conflict, because they are different documents: the
	// namespace, not the key, is what separates them.
}

// --- invisibility ----------------------------------------------------

func TestHostedMemory_UnreachableCollectionIsInvisibleOnTheWritePath(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write", "team-write"},
	}})
	c := f.client(ctx, f.token("alice", "writers"))

	// The tool description must not enumerate "secrets".
	tools, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var saveTool *mcpapi.Tool
	for i := range tools.Tools {
		if tools.Tools[i].Name == toolSaveMemory {
			saveTool = &tools.Tools[i]
		}
	}
	if saveTool == nil {
		t.Fatal("mk_save_memory is not offered to a caller who can write")
	}
	schema, err := json.Marshal(saveTool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(schema), "secrets") {
		t.Errorf("mk_save_memory's definition names a collection the caller cannot reach:\n%s", schema)
	}

	// Naming it must produce the SAME sentence as naming something
	// nobody mounted — no 403, no "you may not", no confirmation that it
	// exists.
	_, hidden, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "title": "T", "content": "x", "collection": "secrets",
	})
	if !isErr {
		t.Fatal("writing to an unreachable collection succeeded")
	}
	_, absent, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "title": "T", "content": "x", "collection": "no-such-collection",
	})
	if !isErr {
		t.Fatal("writing to a collection nobody mounted succeeded")
	}
	normalise := func(s string) string { return strings.ReplaceAll(s, "secrets", "no-such-collection") }
	if normalise(hidden) != absent {
		t.Errorf("a hidden collection errors differently from an absent one:\n hidden: %s\n absent: %s", hidden, absent)
	}
	if strings.Contains(absent, "secrets") {
		t.Errorf("the error for an absent collection names the hidden one: %s", absent)
	}

	// And nothing was written into the hidden collection's store.
	if entries, _ := filepath.Glob(filepath.Join(f.secretsDir, "*", "*")); len(entries) > 0 {
		t.Errorf("the hidden collection's memory store was written to: %v", entries)
	}
}

func TestHostedMemory_ToolIsAbsentWithoutSomewhereToWrite(t *testing.T) {
	ctx := context.Background()

	// A reader holds no write capability anywhere, so mk_save_memory is
	// not offered at all — and mcp-go applies the filter to tools/call
	// too, so it cannot be invoked either.
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"readers"}, Collections: []string{"notes"}, Capabilities: []string{"read"},
	}})
	c := f.client(ctx, f.token("rita", "readers"))
	tools, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == toolSaveMemory {
			t.Fatal("mk_save_memory is offered to a caller who holds no write capability")
		}
	}

	// A registry where no collection has a store never registers the
	// tool in the first place.
	plain := newHostedFixture(t, []authz.Rule{{Collections: []string{"*"}, Capabilities: []string{"admin"}}})
	pc := plain.client(ctx, plain.token("admin", "any"))
	tools, err = pc.ListTools(ctx, mcpapi.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == toolSaveMemory {
			t.Fatal("mk_save_memory is offered by a deployment that configured no memory store")
		}
	}
}

func TestHostedMemory_WriteOnlyGrantSeesTheMemoryToolButNoKBTools(t *testing.T) {
	ctx := context.Background()
	// A rule granting personal-write and NOT read: unusual, but coherent
	// ("drop notes here, don't read the others'"). The caller must pass
	// the gate, be offered mk_save_memory, and be offered nothing else.
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"dropbox"}, Collections: []string{"notes"}, Capabilities: []string{"personal-write"},
	}})
	c := f.client(ctx, f.token("wanda", "dropbox"))

	tools, err := c.ListTools(ctx, mcpapi.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != toolSaveMemory {
		t.Fatalf("tools = %v, want just [%s]", names, toolSaveMemory)
	}

	if _, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "personal", "title": "Dropped", "content": "a note",
	}); isErr {
		t.Fatalf("a write-only caller could not write: %s", text)
	}
}

// --- capability enforcement ------------------------------------------

func TestHostedMemory_CapabilityEnforcementPerScope(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{
		{Groups: []string{"personal-only"}, Collections: []string{"notes"}, Capabilities: []string{"read", "personal-write"}},
		{Groups: []string{"team"}, Collections: []string{"notes"}, Capabilities: []string{"read", "team-write"}},
		{Groups: []string{"global"}, Collections: []string{"notes"}, Capabilities: []string{"read", "global-write"}},
		{Groups: []string{"admins"}, Collections: []string{"notes"}, Capabilities: []string{"admin"}},
		{Groups: []string{"readers"}, Collections: []string{"notes"}, Capabilities: []string{"read"}},
	})

	tests := []struct {
		group  string
		scope  string
		want   string // "saved" | "staged" | "" (refused)
		reason string
	}{
		{"personal-only", "personal", "saved", ""},
		{"personal-only", "team", "staged", "a personal writer proposes rather than publishes"},
		{"personal-only", "global", "staged", ""},
		{"team", "team", "saved", ""},
		{"team", "personal", "", "personal needs personal-write, and has no review path"},
		{"team", "global", "staged", ""},
		{"global", "global", "saved", ""},
		{"global", "team", "staged", ""},
		{"admins", "personal", "saved", "admin implies every capability"},
		{"admins", "team", "saved", ""},
		{"admins", "global", "saved", "including global-write, which admin implies"},
	}
	for _, tc := range tests {
		t.Run(tc.group+"/"+tc.scope, func(t *testing.T) {
			c := f.client(ctx, f.token(tc.group+"-user", tc.group))
			got, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
				"scope": tc.scope, "key": tc.group + "-" + tc.scope,
				"title": "T", "content": "content", "collection": "notes",
			})
			if tc.want == "" {
				if !isErr {
					t.Fatalf("save succeeded, want a refusal (%s)", tc.reason)
				}
				return
			}
			if isErr {
				t.Fatalf("save was refused, want %q (%s): %s", tc.want, tc.reason, text)
			}
			if status, _ := got["status"].(string); status != tc.want {
				t.Errorf("status = %q, want %q (%s)", status, tc.want, tc.reason)
			}
		})
	}

	// A reader is refused one step EARLIER than the others: the tool
	// filter removes mk_save_memory from a caller with no write
	// capability anywhere, and mcp-go applies that filter to tools/call
	// as well as tools/list, so the call is rejected by the transport
	// rather than by the handler. That is a stronger boundary than a
	// refusal, and asserting it here keeps it from silently weakening
	// into one.
	t.Run("readers/every-scope", func(t *testing.T) {
		c := f.client(ctx, f.token("reader-user", "readers"))
		for _, scope := range memory.ScopeNames() {
			res, err := c.CallTool(ctx, mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{
				Name:      toolSaveMemory,
				Arguments: map[string]any{"scope": scope, "title": "T", "content": "x", "collection": "notes"},
			}})
			if err == nil {
				t.Errorf("scope %s: a reader could invoke mk_save_memory (result %+v)", scope, res)
				continue
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("scope %s: err = %v, want the tool to be absent", scope, err)
			}
		}
	})
}

func TestHostedMemory_GlobalWriteIsNotImpliedByTeamWrite(t *testing.T) {
	ctx := context.Background()
	// The reason global-write is a capability of its own: adding it to
	// admin's implications would have retroactively widened every admin
	// rule, and folding it into team-write would widen every team rule.
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"team"}, Collections: []string{"notes"}, Capabilities: []string{"read", "team-write"},
	}})
	c := f.client(ctx, f.token("t", "team"))
	got, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "global", "title": "Policy", "content": "everyone must read this",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if status, _ := got["status"].(string); status != "staged" {
		t.Fatalf("status = %q, want staged — team-write must not confer global-write", status)
	}
}

// --- staging ---------------------------------------------------------

func TestHostedMemory_StagedMemoryIsNeitherSearchableNorShowable(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write"},
	}})
	c := f.client(ctx, f.token("alice", "writers"))

	got, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "global", "key": "policy", "title": "Policy",
		"content": "the unreviewed pangolin claim",
	})
	if isErr {
		t.Fatalf("save: %s", text)
	}
	if status, _ := got["status"].(string); status != "staged" {
		t.Fatalf("status = %q, want staged", status)
	}
	if searchable, _ := got["searchable"].(bool); searchable {
		t.Error("a staged memory reports itself as searchable")
	}
	location, _ := got["location"].(string)
	if !strings.Contains(location, memory.StagingPrefix) {
		t.Errorf("location = %q, want it under %s", location, memory.StagingPrefix)
	}
	// The response tells the caller where it landed, which is what makes
	// the review path actionable rather than a silent drop.
	if note, _ := got["note"].(string); !strings.Contains(note, "pending review") {
		t.Errorf("note = %q, want it to explain the pending state", note)
	}

	// Not in search, not in show, not in list.
	hits, _ := callText(t, ctx, c, toolSearch, map[string]any{"query": "pangolin"})
	if strings.Contains(hits, "pangolin") || strings.Contains(hits, "memory/global/policy") {
		t.Errorf("a staged memory is searchable: %s", hits)
	}
	shown, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": "memory/global/policy"})
	if !isErr {
		t.Errorf("a staged memory is showable: %s", shown)
	}
	listed, _ := callText(t, ctx, c, toolList, map[string]any{"prefix": "memory/"})
	if strings.Contains(listed, "memory/global/policy") {
		t.Errorf("a staged memory is listed: %s", listed)
	}

	// It IS on disk, under the staging prefix, so a reviewer can find it.
	staged, err := filepath.Glob(filepath.Join(f.notesDir, memory.StagingPrefix, "global", "*", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 {
		t.Fatalf("staging area holds %v, want exactly one artifact", staged)
	}
}

// --- immediate searchability -----------------------------------------

func TestHostedMemory_SavedMemoryIsImmediatelySearchableAndShowable(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write", "team-write"},
	}})
	c := f.client(ctx, f.token("alice", "writers"))

	// Nothing yet.
	before, _ := callText(t, ctx, c, toolSearch, map[string]any{"query": "capybara"})
	if strings.Contains(before, "capybara") {
		t.Fatalf("the term is already present: %s", before)
	}

	got, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "key": "fauna", "title": "Fauna notes",
		"content": "The capybara is the office mascot.", "tags": []any{"trivia"},
	})
	if isErr {
		t.Fatalf("save: %s", text)
	}
	if searchable, _ := got["searchable"].(bool); !searchable {
		t.Error("a saved memory reports itself as not searchable")
	}
	id, _ := got["id"].(string)
	if id != "memory/team/fauna" {
		t.Fatalf("id = %q, want memory/team/fauna", id)
	}

	// Same session, immediately after: no restart, no rebuild.
	after, _ := callText(t, ctx, c, toolSearch, map[string]any{"query": "capybara"})
	if !strings.Contains(after, id) {
		t.Fatalf("the memory is not searchable right after the save: %s", after)
	}
	shown, isErr := callText(t, ctx, c, toolShow, map[string]any{"id": id})
	if isErr {
		t.Fatalf("mk_show of a just-saved memory failed: %s", shown)
	}
	for _, want := range []string{"office mascot", "Fauna notes", "memory_scope", "trivia"} {
		if !strings.Contains(shown, want) {
			t.Errorf("mk_show output is missing %q:\n%s", want, shown)
		}
	}
	listed, _ := callText(t, ctx, c, toolList, map[string]any{"prefix": "memory/"})
	if !strings.Contains(listed, id) {
		t.Errorf("mk_list --prefix memory/ does not include the memory: %s", listed)
	}

	// A SECOND, independent session sees it too: the overlay lives on
	// the shared *Collection, not in per-session state.
	other := f.client(ctx, f.token("bob", "writers"))
	fromOther, _ := callText(t, ctx, other, toolSearch, map[string]any{"query": "capybara"})
	if !strings.Contains(fromOther, id) {
		t.Errorf("another session cannot see the saved team memory: %s", fromOther)
	}
}

func TestHostedMemory_APersonalMemoryIsVisibleToTheCollectionsReaders(t *testing.T) {
	ctx := context.Background()
	// Personal scope is about WHO MAY WRITE it, not about a private
	// read channel: meerkat's unit of read authorization is the
	// collection (see docs/design/hosted-mcp.md), and this test pins
	// that the memory feature did not quietly claim otherwise.
	f := newMemFixture(t, []authz.Rule{
		{Groups: []string{"writers"}, Collections: []string{"notes"}, Capabilities: []string{"read", "personal-write"}},
		{Groups: []string{"readers"}, Collections: []string{"notes"}, Capabilities: []string{"read"}},
	})
	writer := f.client(ctx, f.token("alice", "writers"))
	got, text, isErr := callSaveMemory(t, ctx, writer, map[string]any{
		"scope": "personal", "title": "Mine", "content": "the axolotl detail",
	})
	if isErr {
		t.Fatalf("save: %s", text)
	}
	id, _ := got["id"].(string)

	reader := f.client(ctx, f.token("rita", "readers"))
	found, _ := callText(t, ctx, reader, toolSearch, map[string]any{"query": "axolotl"})
	if !strings.Contains(found, id) {
		t.Errorf("a reader of the collection cannot see a personal memory in it: %s", found)
	}
}

// --- optimistic locking ----------------------------------------------

func TestHostedMemory_SecondSaveWithoutAVersionIsAConflict(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "team-write"},
	}})
	c := f.client(ctx, f.token("alice", "writers"))

	first, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "key": "runbook", "title": "Runbook", "content": "version one",
	})
	if isErr {
		t.Fatalf("first save: %s", text)
	}
	version, _ := first["version"].(string)
	if version == "" {
		t.Fatal("first save returned no version")
	}

	// No version, no replace: create-only, so this is a conflict rather
	// than a silent overwrite.
	_, conflictText, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "key": "runbook", "title": "Runbook", "content": "version two",
	})
	if !isErr {
		t.Fatal("a blind second save succeeded, want a conflict")
	}
	if !strings.Contains(conflictText, "conflict") || !strings.Contains(conflictText, version) {
		t.Errorf("conflict message should name the current version %q so a retry works:\n%s", version, conflictText)
	}

	// With the version, it is an update.
	updated, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "key": "runbook", "title": "Runbook", "content": "version two", "version": version,
	})
	if isErr {
		t.Fatalf("conditional update: %s", text)
	}
	if v2, _ := updated["version"].(string); v2 == version {
		t.Error("the version did not change after an update")
	}

	// A stale version is refused.
	if _, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "key": "runbook", "title": "Runbook", "content": "version three", "version": version,
	}); !isErr {
		t.Fatalf("an update from a stale version succeeded: %s", text)
	}

	// replace: true does the read for the caller — still a
	// compare-and-swap, just one they did not have to carry a token for.
	if _, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "key": "runbook", "title": "Runbook", "content": "version four", "replace": true,
	}); isErr {
		t.Fatalf("replace: %s", text)
	}
	shown, _ := callText(t, ctx, c, toolShow, map[string]any{"id": "memory/team/runbook"})
	if !strings.Contains(shown, "version four") {
		t.Errorf("replace did not take effect: %s", shown)
	}
}

func TestHostedMemory_ConcurrentWritersToOneKeyLoseNothingSilently(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "team-write"},
	}})

	const writers = 8
	clients := make([]*mcpclient.Client, writers)
	for i := range clients {
		clients[i] = f.client(ctx, f.token("writer", "writers"))
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		saved     int
		conflicts int
	)
	start := make(chan struct{})
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *mcpclient.Client) {
			defer wg.Done()
			<-start
			res, err := c.CallTool(ctx, mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{
				Name: toolSaveMemory,
				Arguments: map[string]any{
					"scope": "team", "key": "hot", "title": "Hot",
					"content": "writer " + string(rune('a'+i)) + " was here",
				},
			}})
			if err != nil {
				t.Errorf("call: %v", err)
				return
			}
			var sb strings.Builder
			for _, content := range res.Content {
				if tc, ok := content.(mcpapi.TextContent); ok {
					sb.WriteString(tc.Text)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case !res.IsError:
				saved++
			case strings.Contains(sb.String(), "conflict"):
				conflicts++
			default:
				t.Errorf("unexpected tool error: %s", sb.String())
			}
		}(i, c)
	}
	close(start)
	wg.Wait()

	// Exactly one write wins; every other writer is TOLD it lost,
	// instead of believing it succeeded while its memory was discarded.
	if saved != 1 {
		t.Errorf("saved = %d, want exactly 1", saved)
	}
	if conflicts != writers-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, writers-1)
	}
}

// --- anonymous callers -----------------------------------------------

func TestHostedMemory_AnonymousPersonalWriteIsRefusedButTeamIsNot(t *testing.T) {
	ctx := context.Background()
	// No auth: block at all — the unauthenticated hosted shape. meerkat
	// genuinely does not know who is calling, so a personal namespace
	// cannot be derived and the write is refused rather than pooled.
	f := newMemFixture(t, nil)
	c := f.client(ctx, "")

	_, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "personal", "title": "Mine", "content": "x", "collection": "notes",
	})
	if !isErr {
		t.Fatalf("an anonymous personal write succeeded on the hosted transport: %s", text)
	}
	if !strings.Contains(text, "verified identity") {
		t.Errorf("refusal = %q, want it to explain the missing identity", text)
	}

	// A team memory needs no identity, so it still works.
	if _, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "team", "title": "Ours", "content": "x", "collection": "notes",
	}); isErr {
		t.Fatalf("an anonymous team write was refused: %s", text)
	}
}

func TestStdioMemory_AnonymousPersonalWriteIsAllowed(t *testing.T) {
	ctx := context.Background()
	// The stdio transport was spawned by the one user it serves, so a
	// personal memory there has an unambiguous owner even with no token.
	dir := filepath.Join(t.TempDir(), "memory")
	reg, err := collections.New(withMemoryStore(t, collections.FromPages("notes", nil), dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	res, err := saveMemoryHandler(reg, memoryOptions{AllowAnonymousPersonal: true})(ctx, callTool(map[string]any{
		"scope": "personal", "title": "Mine", "content": "a local note",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("stdio personal write refused: %s", resultText(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if ns, _ := out["namespace"].(string); ns != "local" {
		t.Errorf("namespace = %q, want the fixed anonymous one", ns)
	}
}

// --- argument validation ----------------------------------------------

func TestHostedMemory_ArgumentValidation(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes", "secrets"},
		Capabilities: []string{"read", "team-write"},
	}})
	c := f.client(ctx, f.token("alice", "writers"))

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"no scope", map[string]any{"title": "T", "content": "x", "collection": "notes"}},
		{"unknown scope", map[string]any{"scope": "everyone", "title": "T", "content": "x", "collection": "notes"}},
		{"scope naming a capability", map[string]any{"scope": "admin", "title": "T", "content": "x", "collection": "notes"}},
		{"no title", map[string]any{"scope": "team", "content": "x", "collection": "notes"}},
		{"no content", map[string]any{"scope": "team", "title": "T", "collection": "notes"}},
		{"blank content", map[string]any{"scope": "team", "title": "T", "content": "   ", "collection": "notes"}},
		{"unusable key and title", map[string]any{"scope": "team", "title": "...", "key": "///", "content": "x", "collection": "notes"}},
		{"ambiguous collection", map[string]any{"scope": "team", "title": "T", "content": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, text, isErr := callSaveMemory(t, ctx, c, tc.args); !isErr {
				t.Errorf("save succeeded, want a rejected request: %s", text)
			}
		})
	}

	// The ambiguity error names the candidates so the caller can fix it.
	_, text, _ := callSaveMemory(t, ctx, c, map[string]any{"scope": "team", "title": "T", "content": "x"})
	for _, want := range []string{"notes", "secrets", "collection"} {
		if !strings.Contains(text, want) {
			t.Errorf("ambiguity error %q does not mention %q", text, want)
		}
	}
}
