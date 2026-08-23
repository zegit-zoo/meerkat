package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// collections_test.go drives a multi-collection content-source.yaml
// through the real cobra command tree, the way content_source_test.go
// drives the single-source one. What's under test is the wiring:
// resolution -> registry -> --collection / qualified IDs on
// search/show/list, and what `mk version` reports.

// newNamedKBDir builds a minimal content-repo-layout directory whose
// pages are named after the collection it's meant to be mounted as, plus
// a "shared/overview" page every collection has (so the ambiguity rules
// have something to bite on).
func newNamedKBDir(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "wiki", name, "topic.md"),
		"---\nid: "+name+"/topic\ntitle: "+name+" Topic\ncategory: concepts\n---\n# "+name+" Topic\n\nBody about "+name+" widgets.\n")
	write(t, filepath.Join(root, "wiki", "shared", "overview.md"),
		"---\nid: shared/overview\ntitle: "+name+" Overview\n---\n# Overview\n\nThe "+name+" overview.\n")
	return root
}

// multiCollectionConfig writes a two-collection content-source.yaml and
// returns its path.
func multiCollectionConfig(t *testing.T) string {
	t.Helper()
	runbooks := newNamedKBDir(t, "runbooks")
	architecture := newNamedKBDir(t, "architecture")
	path := filepath.Join(t.TempDir(), "content-source.yaml")
	write(t, path, "collections:\n"+
		"  - name: runbooks\n    type: local\n    path: "+runbooks+"\n"+
		"  - name: architecture\n    type: local\n    path: "+architecture+"\n")
	return path
}

func TestCollections_ListSpansAllAndQualifiesIDs(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	out, err := execRoot(t, "--content-source", cfg, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, want := range []string{"runbooks:runbooks/topic", "architecture:architecture/topic"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing qualified id %q:\n%s", want, out)
		}
	}
}

func TestCollections_ListJSONCarriesCollectionAndUnqualifiedID(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	out, err := execRoot(t, "--content-source", cfg, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	found := false
	for _, e := range entries {
		if e["id"] == "runbooks/topic" {
			found = true
			if e["collection"] != "runbooks" {
				t.Errorf("collection = %v, want runbooks", e["collection"])
			}
		}
	}
	if !found {
		t.Errorf("no entry with the unqualified id runbooks/topic (--json must not rewrite ids):\n%s", out)
	}
}

func TestCollections_ListNarrowedByFlag(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	out, err := execRoot(t, "--content-source", cfg, "list", "--collection", "runbooks")
	if err != nil {
		t.Fatalf("list --collection: %v\n%s", err, out)
	}
	if !strings.Contains(out, "runbooks:runbooks/topic") {
		t.Errorf("expected the runbooks page:\n%s", out)
	}
	if strings.Contains(out, "architecture/topic") {
		t.Errorf("--collection runbooks leaked an architecture page:\n%s", out)
	}
}

func TestCollections_UnknownCollectionErrorsWithTheAvailableOnes(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	_, err := execRoot(t, "--content-source", cfg, "list", "--collection", "nope")
	if err == nil {
		t.Fatal("expected an error for an unknown collection")
	}
	for _, want := range []string{"nope", "runbooks", "architecture"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v should mention %q", err, want)
		}
	}
}

func TestCollections_ListCollectionsEnumeratesWhatIsMounted(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	out, err := execRoot(t, "--content-source", cfg, "list", "--collections", "--json")
	if err != nil {
		t.Fatalf("list --collections --json: %v\n%s", err, out)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d collections, want 2:\n%s", len(entries), out)
	}
	if entries[0]["name"] != "runbooks" || entries[1]["name"] != "architecture" {
		t.Errorf("names/order = %v/%v, want runbooks then architecture", entries[0]["name"], entries[1]["name"])
	}
	if entries[0]["type"] != "local" {
		t.Errorf("type = %v, want local", entries[0]["type"])
	}
	if n, _ := entries[0]["pages"].(float64); int(n) != 2 {
		t.Errorf("pages = %v, want 2", entries[0]["pages"])
	}
}

func TestCollections_SearchSpansAllThenNarrows(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	all, err := execRoot(t, "--content-source", cfg, "search", "widgets")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, all)
	}
	for _, want := range []string{"runbooks:runbooks/topic", "architecture:architecture/topic"} {
		if !strings.Contains(all, want) {
			t.Errorf("cross-collection search missing %q:\n%s", want, all)
		}
	}

	narrowed, err := execRoot(t, "--content-source", cfg, "search", "widgets", "--collection", "architecture")
	if err != nil {
		t.Fatalf("search --collection: %v\n%s", err, narrowed)
	}
	if strings.Contains(narrowed, "runbooks:") {
		t.Errorf("--collection architecture leaked a runbooks hit:\n%s", narrowed)
	}
}

// TestCollections_ShowAmbiguousIDIsAnError is the routing rule that has
// to be right: a page ID present in both collections must not silently
// resolve to whichever is configured first.
func TestCollections_ShowAmbiguousIDIsAnError(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	_, err := execRoot(t, "--content-source", cfg, "show", "shared/overview")
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	for _, want := range []string{"runbooks:shared/overview", "architecture:shared/overview"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v should offer the qualified id %q", err, want)
		}
	}
}

func TestCollections_ShowQualifiedAndFlagBothResolve(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	byQualified, err := execRoot(t, "--content-source", cfg, "show", "architecture:shared/overview")
	if err != nil {
		t.Fatalf("show qualified: %v\n%s", err, byQualified)
	}
	if !strings.Contains(byQualified, "The architecture overview") {
		t.Errorf("qualified id served the wrong page:\n%s", byQualified)
	}

	byFlag, err := execRoot(t, "--content-source", cfg, "show", "shared/overview", "--collection", "runbooks")
	if err != nil {
		t.Fatalf("show --collection: %v\n%s", err, byFlag)
	}
	if !strings.Contains(byFlag, "The runbooks overview") {
		t.Errorf("--collection served the wrong page:\n%s", byFlag)
	}
}

func TestCollections_ShowJSONCarriesCollection(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	out, err := execRoot(t, "--content-source", cfg, "show", "runbooks:runbooks/topic", "--json")
	if err != nil {
		t.Fatalf("show --json: %v\n%s", err, out)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if parsed["collection"] != "runbooks" {
		t.Errorf("collection = %v, want runbooks", parsed["collection"])
	}
	if parsed["id"] != "runbooks/topic" {
		t.Errorf("id = %v, want the unqualified page id", parsed["id"])
	}
}

func TestCollections_VersionReportsEveryCollection(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	out, err := execRoot(t, "--content-source", cfg, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if info.KBSource != "collections:2" {
		t.Errorf("kb_source = %q, want collections:2", info.KBSource)
	}
	if len(info.Collections) != 2 {
		t.Fatalf("collections = %+v, want 2 entries", info.Collections)
	}
	if info.Collections[0].Name != "runbooks" || info.Collections[1].Name != "architecture" {
		t.Errorf("collection order = %q/%q, want configuration order",
			info.Collections[0].Name, info.Collections[1].Name)
	}
	for _, c := range info.Collections {
		if !strings.HasPrefix(c.Source, "disk:") {
			t.Errorf("collection %q source = %q, want a disk: provenance", c.Name, c.Source)
		}
	}
}

// TestCollections_SingleSourceVersionUnchanged is the back-compat
// counterpart: a single-source config must still report its own
// provenance in kb_source (not "collections:1"), with exactly one
// collection named "default" itemised alongside.
func TestCollections_SingleSourceVersionUnchanged(t *testing.T) {
	resetKBToEmbedded(t)
	fixture := newFixtureKBDir(t)
	cfg := filepath.Join(t.TempDir(), "content-source.yaml")
	write(t, cfg, "content:\n  type: local\n  path: "+fixture+"\n")

	out, err := execRoot(t, "--content-source", cfg, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if want := "disk:" + fixture; info.KBSource != want {
		t.Errorf("kb_source = %q, want %q", info.KBSource, want)
	}
	if len(info.Collections) != 1 || info.Collections[0].Name != "default" {
		t.Errorf("collections = %+v, want one named default", info.Collections)
	}
}

// TestCollections_SingleSourceIDsStayUnqualified: with one collection,
// nothing anywhere gets a "<collection>:" prefix — the output an
// existing deployment sees is byte-identical.
func TestCollections_SingleSourceIDsStayUnqualified(t *testing.T) {
	resetKBToEmbedded(t)
	fixture := newFixtureKBDir(t)
	cfg := filepath.Join(t.TempDir(), "content-source.yaml")
	write(t, cfg, "content:\n  type: local\n  path: "+fixture+"\n")

	out, err := execRoot(t, "--content-source", cfg, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "concepts/widgets") {
		t.Fatalf("fixture page missing:\n%s", out)
	}
	if strings.Contains(out, "default:") {
		t.Errorf("single-collection output must not qualify ids:\n%s", out)
	}
}

// TestCollections_FirstCollectionBacksTheProcessGlobals documents the
// deliberate compromise for surfaces that aren't collection-aware yet
// (internal/sources, `mk ingest`, shell completion): they read the FIRST
// configured collection.
func TestCollections_FirstCollectionBacksTheProcessGlobals(t *testing.T) {
	resetKBToEmbedded(t)
	cfg := multiCollectionConfig(t)

	if _, err := execRoot(t, "--content-source", cfg, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	ids, _ := completePageIDs(false)(nil, nil, "")
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "runbooks/topic") {
		t.Errorf("completion should see the first collection's pages, got %v", ids)
	}
	if strings.Contains(joined, "architecture/topic") {
		t.Errorf("completion unexpectedly saw a later collection's pages: %v", ids)
	}
}
