package http

import (
	"context"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// newColdTreeServer serves a two-node tree whose child "vendors" is
// declared mount: lazy, so it starts cold.
func newColdTreeServer(t *testing.T) (*Server, *collections.Registry) {
	t.Helper()
	base := t.TempDir()
	mk := func(name, manifest string) string {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "wiki", name+".md"), []byte("# "+name+"\n"+name+" body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, contentsource.ManifestFile), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	vendors := mk("vendors", "kind: KnowledgeBase\nname: vendors\n")
	root := mk("root", "kind: KnowledgeBase\nname: root\nchildren:\n  - name: vendors\n    source: {type: local, path: "+vendors+"}\n    mount: lazy\n")
	resolved, _, err := contentsource.ResolveTree(context.Background(), contentsource.Source{Type: contentsource.TypeLocal, Path: root, Layout: contentsource.MergeLayout(contentsource.Layout{})}, filepath.Join(base, "content-source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := collections.Open(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := &Server{
		cfg: Config{APIKey: "test-key", Version: "test", QueryTimeout: search.DefaultQueryTimeout},
		reg: reg, mux: nethttp.NewServeMux(),
	}
	s.routes()
	return s, reg
}

// TestShowAndList_CancelledRequestLeavesColdChildCold pins item 3 of
// meerkat-mob#63 on the HTTP surface: /show and /list mount a cold
// child under the request's context, so a request that has already
// ended mounts nothing, and a live one still mounts and answers.
func TestShowAndList_CancelledRequestLeavesColdChildCold(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/show", `{"collection":"vendors","id":"vendors"}`},
		{"/list", `{"collection":"vendors"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			srv, reg := newColdTreeServer(t)
			child, err := reg.Get("vendors")
			if err != nil || !child.IsCold() {
				t.Fatalf("vendors must start cold: %v", err)
			}
			post := func(ctx context.Context) *httptest.ResponseRecorder {
				req := httptest.NewRequest(nethttp.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer test-key")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				return rec
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if rec := post(ctx); rec.Code == nethttp.StatusOK {
				t.Errorf("cancelled %s answered 200: %s", tc.path, rec.Body.String())
			}
			if !child.IsCold() {
				t.Fatalf("a cancelled %s must leave the child cold", tc.path)
			}
			if rec := post(context.Background()); rec.Code != nethttp.StatusOK || !strings.Contains(rec.Body.String(), "vendors") {
				t.Errorf("live %s = %d %s, want a mounted answer", tc.path, rec.Code, rec.Body.String())
			}
			if child.IsCold() {
				t.Errorf("a live %s must mount the child", tc.path)
			}
		})
	}
}
