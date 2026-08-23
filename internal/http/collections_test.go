package http

import (
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// collections_test.go covers the HTTP surface of multi-collection
// routing: the optional "collection" field on every data endpoint, the
// /collections discovery endpoint, and the status codes each routing
// failure maps to.

func page(id, title, body string) kb.Page {
	return kb.Page{ID: id, Title: title, Body: body, Front: kb.Frontmatter{ID: id, Title: title, Category: "concepts"}}
}

// newMultiCollectionServer mounts two collections sharing one page ID.
func newMultiCollectionServer(t *testing.T) *Server {
	t.Helper()
	reg, err := collections.New(
		collections.FromPages("runbooks", []kb.Page{
			page("incidents/paging", "Paging", "who to page during an incident"),
			page("shared/overview", "Runbook Overview", "shared overview text"),
		}),
		collections.FromPages("architecture", []kb.Page{
			page("adr/0001", "ADR 1", "we chose object storage for incidents"),
			page("shared/overview", "Architecture Overview", "shared overview text"),
		}),
	)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	s := &Server{
		cfg: Config{APIKey: "test-key", Version: "test", QueryTimeout: search.DefaultQueryTimeout},
		reg: reg, mux: nethttp.NewServeMux(),
	}
	s.routes()
	t.Cleanup(func() { _ = reg.Close() })
	return s
}

func getPath(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(nethttp.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestCollectionsEndpoint_EnumeratesMountedCollections(t *testing.T) {
	srv := newMultiCollectionServer(t)
	rec := getPath(t, srv, "/collections")
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out []collectionEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, rec.Body.String())
	}
	if len(out) != 2 || out[0].Name != "runbooks" || out[1].Name != "architecture" {
		t.Fatalf("got %+v, want runbooks then architecture", out)
	}
	if out[0].Pages != 2 {
		t.Errorf("pages = %d, want 2", out[0].Pages)
	}
}

// TestCollectionsEndpoint_TypeDefaultsToNoneWithoutASource proves GET
// /collections reports "type":"none" for a collection with no
// content-source.yaml entry behind it (here, a FromPages collection, the
// same shape a --kb-dir or the embedded build produces) rather than
// omitting the field — the same default `mk list --collections` and
// mk_list_collections use, so the three surfaces agree on what an
// absent type means.
func TestCollectionsEndpoint_TypeDefaultsToNoneWithoutASource(t *testing.T) {
	srv := newMultiCollectionServer(t)
	rec := getPath(t, srv, "/collections")
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out []collectionEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, rec.Body.String())
	}
	for _, e := range out {
		if e.Type != "none" {
			t.Errorf("collection %q type = %q, want %q", e.Name, e.Type, "none")
		}
	}
}

// TestCollectionsEndpoint_RequiresAuth: which collections a deployment
// mounts is not public information.
func TestCollectionsEndpoint_RequiresAuth(t *testing.T) {
	srv := newMultiCollectionServer(t)
	req := httptest.NewRequest(nethttp.MethodGet, "/collections", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSearch_CollectionFieldNarrows(t *testing.T) {
	srv := newMultiCollectionServer(t)

	rec := postSearch(t, srv, `{"query":"incidents"}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var all []searchHit
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, h := range all {
		seen[h.Collection] = true
	}
	if !seen["runbooks"] || !seen["architecture"] {
		t.Errorf("omitting collection should span all, reached %v", seen)
	}

	rec = postSearch(t, srv, `{"query":"incidents","collection":"runbooks"}`)
	var one []searchHit
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	for _, h := range one {
		if h.Collection != "runbooks" {
			t.Errorf("collection=runbooks leaked a %q hit", h.Collection)
		}
	}
}

func TestSearch_UnknownCollectionIs400(t *testing.T) {
	srv := newMultiCollectionServer(t)
	rec := postSearch(t, srv, `{"query":"x","collection":"nope"}`)
	if rec.Code != nethttp.StatusBadRequest {
		t.Errorf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "runbooks") {
		t.Errorf("error should list the available collections: %s", rec.Body.String())
	}
}

func TestList_CollectionFieldNarrows(t *testing.T) {
	srv := newMultiCollectionServer(t)
	rec := postList(t, srv, `{"collection":"architecture"}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out []listEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d entries, want architecture's 2", len(out))
	}
	for _, e := range out {
		if e.Collection != "architecture" {
			t.Errorf("leaked a %q entry", e.Collection)
		}
	}
}

// TestShow_AmbiguousIs409: the page exists, more than once — a distinct
// condition from "not found" (404) and from a bad request (400).
func TestShow_AmbiguousIs409(t *testing.T) {
	srv := newMultiCollectionServer(t)
	rec := postShow(t, srv, `{"id":"shared/overview"}`)
	if rec.Code != nethttp.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"runbooks:shared/overview", "architecture:shared/overview"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body should offer %q: %s", want, rec.Body.String())
		}
	}
}

func TestShow_QualifiedIDAndCollectionFieldBothResolve(t *testing.T) {
	srv := newMultiCollectionServer(t)

	rec := postShow(t, srv, `{"id":"architecture:shared/overview"}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var byQualified showResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &byQualified); err != nil {
		t.Fatal(err)
	}
	if byQualified.Collection != "architecture" || byQualified.ID != "shared/overview" {
		t.Errorf("qualified id resolved to %+v", byQualified)
	}

	rec = postShow(t, srv, `{"id":"shared/overview","collection":"runbooks"}`)
	var byField showResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &byField); err != nil {
		t.Fatal(err)
	}
	if byField.Collection != "runbooks" {
		t.Errorf("collection field resolved to %+v", byField)
	}
}

// TestOpenAPI_DocumentsCollections proves the published schema carries
// the collection field (and, for a multi-collection deployment, the
// mounted names) so an OpenWebUI-style client can use it.
func TestOpenAPI_DocumentsCollections(t *testing.T) {
	srv := newMultiCollectionServer(t)
	rec := getPath(t, srv, "/openapi.json")
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"/collections"`, `"collection"`, "runbooks", "architecture"} {
		if !strings.Contains(body, want) {
			t.Errorf("openapi.json missing %q", want)
		}
	}
}
