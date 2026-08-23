package collections

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// contract_test.go pins the update contract's two halves: that a
// DECLARED contract is reported verbatim, and that the EFFECTIVE
// contract a caller is handed is the one they can actually act on —
// never a route that would be refused, and never wider than what the
// operator declared.
//
// The last test in the file is the one that matters most: a contract is
// reachable only through the restricted registry view, so a hidden
// collection leaks nothing — including its contribution repo URL.

const handbookRepo = "https://github.com/example-org/handbook.git"

// withContract attaches a source (and so a contract) to a FromPages
// collection.
func withContract(c *Collection, src contentsource.Source) *Collection {
	c.Source = src
	return c
}

// directSource is a writable local backend declaring direct writes.
func directSource(description string) contentsource.Source {
	return contentsource.Source{
		Type:        contentsource.TypeLocal,
		Path:        "../kb",
		Description: description,
		Update: &contentsource.UpdateSpec{
			Method:       contentsource.UpdateDirect,
			Instructions: "Pages live under wiki/.",
		},
	}
}

// mergeRequestSource declares a review flow, on a backend meerkat could
// never write to — the mirror and the contribution repo are different
// addresses, which is the whole point.
func mergeRequestSource() contentsource.Source {
	return contentsource.Source{
		Type:        contentsource.TypeGCS,
		Bucket:      "my-org-knowledge",
		Object:      "bundles/handbook-v3.tar.gz",
		Description: "Engineering handbook.",
		Update: &contentsource.UpdateSpec{
			Method:       contentsource.UpdateMergeRequest,
			Repo:         handbookRepo,
			Host:         contentsource.UpdateHostGitHub,
			Branch:       "main",
			Path:         "wiki",
			Instructions: "Fork, branch, open a PR against main.",
		},
	}
}

// grants builds a caller holding caps over one collection.
func grants(collection string, caps ...authz.Capability) *authz.Grants {
	return authz.NewGrants(
		authz.Identity{Subject: "user-1", Issuer: "https://idp.example.com"},
		map[string][]authz.Capability{collection: caps},
	)
}

func onePage(id string) []kb.Page { return []kb.Page{page(id, "Title", "body")} }

func TestContract_IsWhatTheOperatorDeclared(t *testing.T) {
	c := withContract(FromPages("handbook", onePage("a")), mergeRequestSource())

	got := c.Contract()
	if got.Method != MethodMergeRequest {
		t.Fatalf("method = %q", got.Method)
	}
	if got.Repo != handbookRepo || got.Host != "github" || got.Branch != "main" || got.Path != "wiki" {
		t.Errorf("contract = %+v", got)
	}
	if !got.Declared() {
		t.Error("a merge-request contract is a declared contract")
	}
	if c.Description() != "Engineering handbook." {
		t.Errorf("description = %q", c.Description())
	}
}

func TestContract_IsNeverInferredFromTheSourceType(t *testing.T) {
	// A local directory is writable, and meerkat still does not conclude
	// that writing into it is sanctioned. Only the operator says.
	local := withContract(FromPages("scratch", onePage("a")), contentsource.Source{
		Type: contentsource.TypeLocal,
		Path: "../scratch",
	})
	if got := local.Contract().Method; got != MethodNone {
		t.Errorf("an undeclared contract on a writable backend = %q, want %q", got, MethodNone)
	}
	if local.Contract().Declared() {
		t.Error("Declared() must be false for an undeclared contract")
	}
	// And a collection with no source at all (the embedded fallback, a
	// FromPages registry) reports none rather than panicking on a nil
	// spec.
	if got := FromPages("bare", onePage("a")).Contract().Method; got != MethodNone {
		t.Errorf("a collection with no source = %q, want %q", got, MethodNone)
	}
}

func TestContract_MergeRequestDefaultsAreReapplied(t *testing.T) {
	// Defaults are applied at config load; a hand-assembled Source must
	// still render a contract an agent can act on.
	c := withContract(FromPages("handbook", onePage("a")), contentsource.Source{
		Type:   contentsource.TypeLocal,
		Path:   "../kb",
		Update: &contentsource.UpdateSpec{Method: contentsource.UpdateMergeRequest, Repo: handbookRepo},
	})
	got := c.Contract()
	if got.Branch != contentsource.DefaultUpdateBranch || got.Host != contentsource.UpdateHostOther {
		t.Errorf("contract = %+v, want the branch/host defaults", got)
	}
}

// --- effective rendering ---------------------------------------------

func TestEffectiveContract_DirectForACallerWhoMayPublish(t *testing.T) {
	c := withContract(FromPages("scratch", onePage("a")), directSource("Scratch space."))

	eff := c.EffectiveContract(grants("scratch", authz.CapRead, authz.CapGlobalWrite))
	if eff.Method != MethodDirect {
		t.Fatalf("method = %q, want %q (reason: %s)", eff.Method, MethodDirect, eff.Reason)
	}
	if eff.Declared != MethodDirect {
		t.Errorf("declared = %q", eff.Declared)
	}
	if eff.Description != "Scratch space." || eff.Instructions != "Pages live under wiki/." {
		t.Errorf("effective contract = %+v", eff)
	}
	if !eff.Actionable() {
		t.Error("a direct contract is actionable")
	}

	// admin implies global-write, as it implies everything.
	if got := c.EffectiveContract(grants("scratch", authz.CapAdmin)).Method; got != MethodDirect {
		t.Errorf("an admin's method = %q, want %q", got, MethodDirect)
	}
}

func TestEffectiveContract_MergeRequestNeedsNoCapability(t *testing.T) {
	c := withContract(FromPages("handbook", onePage("a")), mergeRequestSource())

	// A read-only caller is not told "you can do nothing": they are told
	// how to propose. Opening a merge request uses their own forge
	// credentials against a repo meerkat does not serve, so there is no
	// meerkat capability to check.
	eff := c.EffectiveContract(grants("handbook", authz.CapRead))
	if eff.Method != MethodMergeRequest {
		t.Fatalf("method = %q, want %q (reason: %s)", eff.Method, MethodMergeRequest, eff.Reason)
	}
	if eff.Repo != handbookRepo || eff.Host != "github" || eff.Branch != "main" || eff.Path != "wiki" {
		t.Errorf("the merge-request mechanics must be carried through: %+v", eff)
	}
	if !strings.Contains(eff.Instructions, "Fork, branch, open a PR") {
		t.Errorf("instructions = %q", eff.Instructions)
	}
}

// TestEffectiveContract_CapabilitiesNeverWidenTheDeclaration: an
// operator who declared a review flow gets a review flow, even for a
// caller who holds every capability. Effective rendering can only walk a
// caller DOWN the ladder.
func TestEffectiveContract_CapabilitiesNeverWidenTheDeclaration(t *testing.T) {
	c := withContract(FromPages("handbook", onePage("a")), mergeRequestSource())

	eff := c.EffectiveContract(grants("handbook", authz.CapAdmin))
	if eff.Method != MethodMergeRequest {
		t.Fatalf("an admin's method = %q, want the declared %q", eff.Method, MethodMergeRequest)
	}
}

func TestEffectiveContract_FallsBackToStagingForAWriterWhoMayNotPublish(t *testing.T) {
	c := withContract(FromPages("scratch", onePage("a")), directSource(""))
	attachLocalMemory(t, c)

	// personal-write makes this caller a writer in the collection but
	// not a publisher to it, so the sanctioned path is a proposal.
	eff := c.EffectiveContract(grants("scratch", authz.CapRead, authz.CapPersonalWrite))
	if eff.Method != MethodStaging {
		t.Fatalf("method = %q, want %q (reason: %s)", eff.Method, MethodStaging, eff.Reason)
	}
	if eff.Declared != MethodDirect {
		t.Errorf("declared = %q, want the operator's %q", eff.Declared, MethodDirect)
	}
	if !strings.Contains(eff.Reason, string(PublishCapability)) || !strings.Contains(eff.Reason, "personal-write") {
		t.Errorf("the reason should name the capability missing and the ones held: %q", eff.Reason)
	}
	if !strings.Contains(eff.Reason, "mk_save_memory") {
		t.Errorf("the reason should name the path to take instead: %q", eff.Reason)
	}
}

func TestEffectiveContract_NoneForAReaderWithNowhereToPropose(t *testing.T) {
	// Declared direct, no memory store, and a caller who may only read:
	// every rung of the ladder is out.
	c := withContract(FromPages("scratch", onePage("a")), directSource(""))

	eff := c.EffectiveContract(grants("scratch", authz.CapRead))
	if eff.Method != MethodNone {
		t.Fatalf("method = %q, want %q (reason: %s)", eff.Method, MethodNone, eff.Reason)
	}
	if eff.Actionable() {
		t.Error("MethodNone is not actionable")
	}
	if eff.Declared != MethodDirect {
		t.Errorf("declared = %q — the caller should still be able to see what the operator declared", eff.Declared)
	}
	if !strings.Contains(eff.Reason, string(PublishCapability)) {
		t.Errorf("the reason should name the missing capability: %q", eff.Reason)
	}
	// The reason must not leak: it names this collection (which the
	// caller can see) and their own capabilities, and nothing else.
	if strings.Contains(eff.Reason, handbookRepo) {
		t.Errorf("the reason named another collection's contribution repo: %q", eff.Reason)
	}
}

func TestEffectiveContract_UndeclaredStaysNoneEvenWithAMemoryStore(t *testing.T) {
	// method: none is a statement, not an absence of information. A
	// memory store does not turn it into a contribution path — the
	// memory toolset advertises itself separately.
	c := withContract(FromPages("notes", onePage("a")), contentsource.Source{
		Type:   contentsource.TypeLocal,
		Path:   "../notes",
		Update: &contentsource.UpdateSpec{Method: contentsource.UpdateNone},
	})
	attachLocalMemory(t, c)

	eff := c.EffectiveContract(grants("notes", authz.CapRead, authz.CapGlobalWrite))
	if eff.Method != MethodNone {
		t.Fatalf("method = %q, want %q", eff.Method, MethodNone)
	}
	if !strings.Contains(eff.Reason, "declared no update contract") {
		t.Errorf("reason = %q", eff.Reason)
	}
}

// TestEffectiveContract_TwoCallersSeeDifferentContracts is the
// acceptance criterion: the same collection, two grant shapes, two
// different answers.
func TestEffectiveContract_TwoCallersSeeDifferentContracts(t *testing.T) {
	c := withContract(FromPages("scratch", onePage("a")), directSource(""))
	attachLocalMemory(t, c)

	publisher := c.EffectiveContract(grants("scratch", authz.CapRead, authz.CapGlobalWrite))
	writer := c.EffectiveContract(grants("scratch", authz.CapRead, authz.CapPersonalWrite))
	reader := c.EffectiveContract(grants("scratch", authz.CapRead))

	if publisher.Method != MethodDirect || writer.Method != MethodStaging || reader.Method != MethodNone {
		t.Fatalf("three grant shapes must render three paths: publisher=%q writer=%q reader=%q",
			publisher.Method, writer.Method, reader.Method)
	}
	if publisher.Declared != writer.Declared || writer.Declared != reader.Declared {
		t.Error("all three see the same DECLARED contract; only the effective one differs")
	}
}

// TestEffectiveContract_NoGrantsRendersTheDeclaredContract covers
// anonymous/local mode — the CLI, stdio MCP, the static-token HTTP
// server, a hosted server with no auth: block. There is no capability
// gate to fail, so the effective contract is the declared one.
func TestEffectiveContract_NoGrantsRendersTheDeclaredContract(t *testing.T) {
	direct := withContract(FromPages("scratch", onePage("a")), directSource(""))
	if eff := direct.EffectiveContract(nil); eff.Method != MethodDirect || eff.Declared != MethodDirect {
		t.Errorf("nil grants on a direct collection = %q (declared %q), want the declared contract", eff.Method, eff.Declared)
	}
	mr := withContract(FromPages("handbook", onePage("a")), mergeRequestSource())
	if eff := mr.EffectiveContract(nil); eff.Method != MethodMergeRequest || eff.Repo != handbookRepo {
		t.Errorf("nil grants on a merge-request collection = %+v", eff)
	}
	bare := FromPages("bare", onePage("a"))
	if eff := bare.EffectiveContract(nil); eff.Method != MethodNone {
		t.Errorf("nil grants on an undeclared collection = %q, want %q", eff.Method, MethodNone)
	}
}

func TestRegistry_EffectiveContractsAreInConfigurationOrder(t *testing.T) {
	reg := contractRegistry(t)

	got := reg.EffectiveContracts(nil)
	if len(got) != 3 {
		t.Fatalf("got %d contracts, want one per mounted collection", len(got))
	}
	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Collection)
	}
	if strings.Join(names, ",") != "handbook,scratch,secrets" {
		t.Errorf("EffectiveContracts order = %v", names)
	}

	one, err := reg.EffectiveContract("handbook", nil)
	if err != nil {
		t.Fatalf("EffectiveContract: %v", err)
	}
	if one.Method != MethodMergeRequest {
		t.Errorf("handbook method = %q", one.Method)
	}
}

// contractRegistry mounts three collections, the last of which is the
// one a restricted caller must not learn anything about.
func contractRegistry(t *testing.T) *Registry {
	t.Helper()
	secrets := contentsource.Source{
		Type:        contentsource.TypeLocal,
		Path:        "../secrets",
		Description: "Compensation and payroll.",
		Update: &contentsource.UpdateSpec{
			Method:       contentsource.UpdateMergeRequest,
			Repo:         "https://github.com/example-org/secret-payroll-kb.git",
			Host:         contentsource.UpdateHostGitHub,
			Branch:       "main",
			Path:         "wiki",
			Instructions: "Ask the compensation committee first.",
		},
	}
	reg, err := New(
		withContract(FromPages("handbook", onePage("handbook/index")), mergeRequestSource()),
		withContract(FromPages("scratch", onePage("scratch/index")), directSource("Scratch space.")),
		withContract(FromPages("secrets", onePage("payroll/salaries")), secrets),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// TestRestrict_ExposesNoHiddenContract is the leak test. A contract is
// reachable only through a *Collection, and a *Collection only through a
// registry — so the restricted view is the whole enforcement mechanism,
// exactly as it is for search, show and list.
func TestRestrict_ExposesNoHiddenContract(t *testing.T) {
	const hiddenRepo = "https://github.com/example-org/secret-payroll-kb.git"
	view := contractRegistry(t).Restrict(only("handbook", "scratch"))
	g := authz.NewGrants(
		authz.Identity{Subject: "user-1", Issuer: "https://idp.example.com"},
		map[string][]authz.Capability{"handbook": {authz.CapRead}, "scratch": {authz.CapRead}},
	)

	contracts := view.EffectiveContracts(g)
	if len(contracts) != 2 {
		t.Fatalf("got %d contracts, want the caller's 2", len(contracts))
	}
	for _, e := range contracts {
		if e.Collection == "secrets" {
			t.Fatal("EffectiveContracts enumerated a hidden collection")
		}
	}
	// Not just the name: nothing about the hidden collection — its
	// description, its contribution repo, its instructions — may appear
	// anywhere in the rendered result.
	var b strings.Builder
	for _, e := range contracts {
		b.WriteString(e.Collection + "\x00" + e.Description + "\x00" + e.Repo + "\x00" + e.Instructions + "\x00" + e.Reason + "\x00")
	}
	for _, leak := range []string{hiddenRepo, "secret-payroll-kb", "Compensation and payroll.", "compensation committee", "secrets"} {
		if strings.Contains(b.String(), leak) {
			t.Errorf("a hidden collection's contract leaked %q into a restricted view:\n%s", leak, b.String())
		}
	}

	// Asking for it by name is the same error a name nobody ever mounted
	// produces — the contract accessor routes through Get, so it
	// inherits that.
	hidden, hiddenErr := view.EffectiveContract("secrets", g)
	absent, absentErr := view.EffectiveContract("no-such-collection", g)
	if !errors.Is(hiddenErr, ErrUnknownCollection) || !errors.Is(absentErr, ErrUnknownCollection) {
		t.Fatalf("both should wrap ErrUnknownCollection: %v / %v", hiddenErr, absentErr)
	}
	if hidden.Method != "" || absent.Method != "" {
		t.Errorf("a failed lookup must return the zero contract, got %+v / %+v", hidden, absent)
	}
	normalise := func(err error, name string) string { return strings.ReplaceAll(err.Error(), name, "<name>") }
	if got, want := normalise(hiddenErr, "secrets"), normalise(absentErr, "no-such-collection"); got != want {
		t.Errorf("a hidden collection must error identically to an absent one:\n hidden: %s\n absent: %s", got, want)
	}
	if strings.Contains(hiddenErr.Error(), hiddenRepo) {
		t.Errorf("the error leaked the contribution repo: %v", hiddenErr)
	}
}

// attachLocalMemory gives a collection a writable memory store, which is
// what makes the staging rung of the ladder available.
func attachLocalMemory(t *testing.T, c *Collection) {
	t.Helper()
	store, err := memory.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	if err := c.AttachMemory(context.Background(), store); err != nil {
		t.Fatalf("AttachMemory: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
}
