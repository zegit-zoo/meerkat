package ingest

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// promotionFixture is a three-tier tree: root -> platform -> flux, with
// grafana also at depth 2 but already reachable by a root pointer.
func promotionFixture(t *testing.T) (*collections.Registry, *traversal.Log) {
	t.Helper()
	ctx := context.Background()
	root := collections.FromPages("root", []kb.Page{
		{ID: "platform", Title: "Platform", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:platform", Hint: "Platform hub."}},
		{ID: "grafana", Title: "Grafana", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:grafana", Hint: "Dashboards."}},
	})
	root.Tree = &contentsource.TreeNode{Name: "root", Path: "root", Depth: 0}
	root.Source.Update = &contentsource.UpdateSpec{Method: contentsource.UpdateDirect}
	root.Source.Update.Normalize()
	rootStore, err := memory.OpenLocal(filepath.Join(t.TempDir(), "rootmem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := root.AttachMemory(ctx, rootStore); err != nil {
		t.Fatal(err)
	}
	platform := collections.FromPages("platform", []kb.Page{
		{ID: "flux-ptr", Title: "Flux", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux", Hint: "Flux."}},
	})
	platform.Tree = &contentsource.TreeNode{Name: "platform", Path: "root/platform", Depth: 1, Parent: "root"}
	flux := collections.FromPages("flux", []kb.Page{{ID: "concepts/drift", Title: "Drift"}, {ID: "concepts/sync", Title: "Sync"}})
	flux.Tree = &contentsource.TreeNode{Name: "flux", Path: "root/platform/flux", Depth: 2, Parent: "platform", Description: "Flux CD: sources, kustomizations, drift."}
	grafana := collections.FromPages("grafana", []kb.Page{{ID: "panels", Title: "Panels"}})
	grafana.Tree = &contentsource.TreeNode{Name: "grafana", Path: "root/platform/grafana", Depth: 2, Parent: "platform"}
	reg, err := collections.New(root, platform, flux, grafana)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	log, err := traversal.Open(ctx, &traversal.Config{Backend: "local", Path: t.TempDir(), HMACKeyEnv: "MEERKAT_TEST_PATH_KEY"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// grafana is hotter than flux but already has a root pointer;
	// platform is hot but only one hop down.
	temps := map[string]traversal.Temperature{
		"platform": {Temperature: 90, Depth: 1},
		"grafana":  {Temperature: 50, Depth: 2},
		"flux":     {Temperature: 40, Depth: 2},
	}
	if err := log.RecordTemperatures(ctx, temps); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := log.Record(ctx, traversal.Entry{Session: "s", Outcome: "found", Attempted: []string{"root", "platform", "flux"}, Pages: []string{"flux:concepts/drift"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Record(ctx, traversal.Entry{Session: "s", Outcome: "found", Attempted: []string{"root", "platform", "flux"}, Pages: []string{"flux:concepts/sync", "flux:concepts/drift"}}); err != nil {
		t.Fatal(err)
	}
	return reg, log
}

func TestLibrarian_ProposesRootPointerForHotDeepCollection(t *testing.T) {
	ctx := context.Background()
	reg, log := promotionFixture(t)
	now := time.Now().UTC()
	rep, err := Librarian(ctx, reg, nil, LibrarianOpts{Log: log, Days: 2, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Promotions) != 1 || rep.Count(FindingPromotion) != 1 {
		var b bytes.Buffer
		rep.Write(&b)
		t.Fatalf("promotions = %+v\n%s", rep.Promotions, b.String())
	}
	p := rep.Promotions[0]
	if p.Collection != "flux" || p.Hub != "root" || p.Depth != 2 || p.Temperature != 40 || p.Method != contentsource.UpdateDirect {
		t.Errorf("promotion = %+v", p)
	}
	if len(p.ExistingPointers) != 1 || p.ExistingPointers[0] != "platform:flux-ptr" {
		t.Errorf("existing pointers = %v", p.ExistingPointers)
	}
	if len(p.HotPages) != 2 || p.HotPages[0].ID != "concepts/drift" || p.HotPages[0].Count != 3 || p.HotPages[1].Count != 1 {
		t.Errorf("hot pages = %+v", p.HotPages)
	}
	if p.Hint != "Flux CD: sources, kustomizations, drift." {
		t.Errorf("hint = %q", p.Hint)
	}
	var b bytes.Buffer
	rep.Write(&b)
	if !strings.Contains(b.String(), "promotion") || !strings.Contains(b.String(), "root:pointers/flux") || !strings.Contains(b.String(), "concepts/drift (3)") {
		t.Errorf("report text:\n%s", b.String())
	}
	// Report-only: nothing was written.
	root, _ := reg.Get("root")
	if recs, _ := root.Memory().Load(ctx); len(recs) != 0 {
		t.Fatalf("report must not write: %+v", recs)
	}

	applied, err := Apply(ctx, reg, nil, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Action != "filed" || applied[0].IntakeID != "promote:flux" {
		t.Fatalf("applied = %+v", applied)
	}
	recs, _ := root.Memory().Load(ctx)
	if len(recs) != 1 || recs[0].Key != "global/pointers/flux.md" {
		t.Fatalf("filed = %+v", recs)
	}
	page, err := root.Load("memory/global/pointers/flux")
	if err != nil {
		t.Fatal(err)
	}
	if target, err := page.Pointer(); err != nil || target.String() != "collection:flux" || page.Front.Hint != p.Hint {
		t.Errorf("filed pointer = %+v (%v)", page.Front, err)
	}
	if ptrs := reg.PointersTo("flux"); len(ptrs) != 2 || ptrs[1] != "root:memory/global/pointers/flux" {
		t.Errorf("PointersTo(flux) = %v", ptrs)
	}
	// Idempotent: the root pointer now exists, so the next run proposes
	// nothing and a second Apply writes nothing.
	rep2, err := Librarian(ctx, reg, nil, LibrarianOpts{Log: log, Days: 2, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Promotions) != 0 {
		t.Errorf("proposed again: %+v", rep2.Promotions)
	}
}

func TestLibrarian_PromotionTopNAndMergeRequestHub(t *testing.T) {
	ctx := context.Background()
	reg, log := promotionFixture(t)
	// Make grafana's root pointer disappear by pointing it elsewhere,
	// so two deep collections compete; TopN 1 keeps the hotter one.
	root, _ := reg.Get("root")
	if _, _, err := root.SaveMemory(ctx, "global/grafana-ptr.md", []byte("---\ntype: pointer\ntarget: collection:platform\nhint: x\n---\n# g\n"), memory.CreateOnly()); err != nil {
		t.Fatal(err)
	}
	grafana, _ := reg.Get("grafana")
	grafana.Tree.Parent = "platform"
	// Rebuild: the pointer page named "grafana" in root still exists,
	// so drop it by using a fresh registry without it.
	pages := []kb.Page{{ID: "platform", Title: "Platform", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:platform", Hint: "Platform hub."}}}
	root2 := collections.FromPages("root", pages)
	root2.Tree = root.Tree
	root2.Source.Update = &contentsource.UpdateSpec{Method: contentsource.UpdateMergeRequest, Repo: "https://github.com/example/kb", Branch: "main", Path: "wiki"}
	root2.Source.Update.Normalize()
	platform, _ := reg.Get("platform")
	flux, _ := reg.Get("flux")
	reg2, err := collections.New(root2, platform, flux, grafana)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Librarian(ctx, reg2, nil, LibrarianOpts{Log: log, Days: 2, PromotionTopN: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Promotions) != 1 || rep.Promotions[0].Collection != "grafana" || rep.Promotions[0].Temperature != 50 {
		t.Fatalf("promotions = %+v", rep.Promotions)
	}
	if rep.Promotions[0].Hint != "Knowledge base grafana (root/platform/grafana)." {
		t.Errorf("fallback hint = %q", rep.Promotions[0].Hint)
	}
	applied, err := Apply(ctx, reg2, nil, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Action != "instructions" || !strings.Contains(applied[0].Detail, "pointers/grafana -> collection:grafana") {
		t.Errorf("applied = %+v", applied)
	}
	// Without a log there is no evidence and no proposal.
	rep, err = Librarian(ctx, reg2, nil, LibrarianOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Promotions) != 0 {
		t.Errorf("proposed without evidence: %+v", rep.Promotions)
	}
}
