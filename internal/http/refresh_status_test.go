package http

import (
	"context"
	"encoding/json"
	nethttp "net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/refresh"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// refresh_status_test.go pins #119: `mk http serve` loads its
// collections once and runs no refresh controller, so it must not report
// a refresh status. A status built from configuration alone reads as a
// live cycle that never ran: last_success never moves and nothing is
// ever degraded.

// refreshedLocalServer mounts one `type: local` collection with a
// refresh: block through collections.Open, the path a content-source.yaml
// takes, which is what gives the collection its status slot.
func refreshedLocalServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "wiki", "notes", "p.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("---\nid: notes/p\ntitle: P\n---\n# P\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := collections.Open(context.Background(), []contentsource.ResolvedCollection{{
		Name: "notes", Dir: dir, Provenance: "disk:" + dir,
		Source: contentsource.Source{
			Type: contentsource.TypeLocal, Path: dir, Layout: contentsource.Layout{Wiki: "wiki"},
			Refresh: &refresh.Spec{Interval: refresh.Duration(time.Minute)},
		},
	}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	c, err := reg.Get("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ReloadStatuses()) == 0 {
		t.Fatal("precondition: a collection with a refresh: block has a status slot")
	}
	s := &Server{
		cfg: Config{APIKey: "test-key-0123456789", Version: "test", QueryTimeout: search.DefaultQueryTimeout},
		reg: reg, mux: nethttp.NewServeMux(),
	}
	s.routes()
	return s
}

func TestCollectionsEndpoint_ReportsNoRefreshStatus(t *testing.T) {
	rec := getPath(t, refreshedLocalServer(t), "/collections")
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, rec.Body.String())
	}
	if len(out) != 1 {
		t.Fatalf("got %d entries, want 1: %s", len(out), rec.Body.String())
	}
	for _, key := range []string{"refresh", "freshness"} {
		if v, ok := out[0][key]; ok {
			t.Errorf("GET /collections carries %q = %s; this server never runs a refresh cycle", key, v)
		}
	}
}

func TestOpenAPI_CollectionsSchemaDeclaresNoRefresh(t *testing.T) {
	rec := getPath(t, refreshedLocalServer(t), "/openapi.json")
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Items struct {
								Properties map[string]json.RawMessage `json:"properties"`
							} `json:"items"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	props := doc.Paths["/collections"].Get.Responses["200"].Content["application/json"].Schema.Items.Properties
	if len(props) == 0 {
		t.Fatal("precondition: the /collections response schema lists its properties")
	}
	if _, ok := props["refresh"]; ok {
		t.Error("the /collections schema still declares a refresh property")
	}
}
