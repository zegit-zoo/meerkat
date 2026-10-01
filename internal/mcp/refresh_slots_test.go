package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// refresh_slots_test.go pins #120: mk_list_collections reports a refresh
// status only for a target this process's controller runs. A status slot
// is created from configuration at mount for every refresh: block, but
// stdio runs only the scheduled `type: local` targets (#112, #117), so
// an object store's slot there would never move: no attempt, no
// success, never degraded.

// slotsRegistry mounts "notes", a `type: local` collection with a
// refresh: block (stdio runs it), and "bucket", a GCS collection whose
// content was resolved into a directory at startup and which carries a
// refresh: block (only the hosted transport runs it).
func slotsRegistry(t *testing.T) *collections.Registry {
	t.Helper()
	page := func(dir, id string) {
		p := filepath.Join(dir, "wiki", filepath.FromSlash(id)+".md")
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("---\nid: "+id+"\ntitle: T\n---\n# T\n\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notes, bucket := t.TempDir(), t.TempDir()
	page(notes, "notes/p")
	page(bucket, "handbook/q")
	every := &refresh.Spec{Interval: refresh.Duration(time.Minute)}
	reg, err := collections.Open(context.Background(), []contentsource.ResolvedCollection{
		{Name: "notes", Dir: notes, Provenance: "disk:" + notes, Source: contentsource.Source{
			Type: contentsource.TypeLocal, Path: notes, Layout: contentsource.Layout{Wiki: "wiki"}, Refresh: every,
		}},
		{Name: "bucket", Dir: bucket, Provenance: "gcs://example-kb/handbook/live/", Source: contentsource.Source{
			Type: contentsource.TypeGCS, Bucket: "example-kb", Prefix: "handbook/live/",
			Layout: contentsource.Layout{Wiki: "wiki"}, Refresh: every,
		}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	for _, name := range []string{"notes", "bucket"} {
		c, err := reg.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.ReloadStatuses()) != 1 {
			t.Fatalf("precondition: %s has a content status slot, got %+v", name, c.ReloadStatuses())
		}
	}
	return reg
}

// listedRefreshKinds returns, per collection, the kinds of the refresh
// entries mk_list_collections reports.
func listedRefreshKinds(t *testing.T, reg *collections.Registry, running refreshSlots) map[string][]string {
	t.Helper()
	body, err := listCollectionsJSON(context.Background(), reg, running)
	if err != nil {
		t.Fatal(err)
	}
	var got []collectionSummary
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, c := range got {
		for _, st := range c.Refresh {
			out[c.Name] = append(out[c.Name], st.Kind)
		}
	}
	return out
}

// stdio runs the local collection's cycle and never the bucket's, so
// only the local slot is reported: through stdioOptions, the options
// ServeStdioWith serves with.
func TestStdioListCollections_ReportsOnlyTheSlotsStdioRuns(t *testing.T) {
	reg := slotsRegistry(t)
	got := listedRefreshKinds(t, reg, stdioOptions(reg, OutcomeOptions{}).Refreshing)
	if len(got["notes"]) != 1 || got["notes"][0] != refresh.KindContent {
		t.Errorf("notes refresh = %v, want the content slot stdio runs", got["notes"])
	}
	if kinds, ok := got["bucket"]; ok {
		t.Errorf("bucket reports refresh %v, a target stdio never runs", kinds)
	}
}

// The hosted server runs every target, so it reports both, as before.
// Asked through its own MCP endpoint, so the options NewHosted wires
// into the tool are what is tested.
func TestHostedListCollections_ReportsEverySlotItRuns(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{registry: slotsRegistry(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, isErr := callText(t, ctx, f.client(ctx, "", nil), toolListCollections, map[string]any{})
	if isErr {
		t.Fatalf("mk_list_collections: %s", body)
	}
	var got []collectionSummary
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	kinds := map[string][]string{}
	for _, c := range got {
		for _, st := range c.Refresh {
			kinds[c.Name] = append(kinds[c.Name], st.Kind)
		}
	}
	for _, name := range []string{"notes", "bucket"} {
		if len(kinds[name]) != 1 || kinds[name][0] != refresh.KindContent {
			t.Errorf("%s refresh = %v, want its content slot", name, kinds[name])
		}
	}
}

// With no controller at all, nothing is reported.
func TestListCollections_NoControllerReportsNoRefresh(t *testing.T) {
	if got := listedRefreshKinds(t, slotsRegistry(t), nil); len(got) != 0 {
		t.Errorf("refresh reported with no controller: %v", got)
	}
}

// A slot is a collection AND a kind: stdio runs a local collection's
// content cycle, never a memory store's, so a collection with both
// keeps only the one that runs.
func TestRefreshSlots_MatchOnCollectionAndKind(t *testing.T) {
	running := refreshSlots{{collection: "notes", kind: refresh.KindContent}: true}
	all := []collections.ReloadStatus{{Kind: refresh.KindContent}, {Kind: refresh.KindMemory}}
	if got := running.of("notes", all); len(got) != 1 || got[0].Kind != refresh.KindContent {
		t.Errorf("notes = %+v, want only its content slot", got)
	}
	if got := running.of("other", all); len(got) != 0 {
		t.Errorf("another collection's slots = %+v, want none", got)
	}
}
