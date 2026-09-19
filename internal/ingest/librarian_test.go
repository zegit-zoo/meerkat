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
	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

func librarianFixture(t *testing.T) (*collections.Registry, *intake.Store, *traversal.Log) {
	t.Helper()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	hub := collections.FromPages("hub", []kb.Page{
		{ID: "flux", Title: "Flux", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux", Hint: "Flux.", Related: []string{"missing/page"}}},
		{ID: "old", Title: "Old", Front: kb.Frontmatter{StaleAfter: now.AddDate(0, 0, -200).Format("2006-01-02")}},
		{ID: "recent", Title: "Recent", Front: kb.Frontmatter{StaleAfter: now.AddDate(0, 0, -10).Format("2006-01-02")}},
	})
	flux := collections.FromPages("flux", []kb.Page{{ID: "concepts/drift", Title: "Drift"}})
	flux.Source.Update = &contentsource.UpdateSpec{Method: contentsource.UpdateDirect}
	flux.Source.Update.Normalize()
	fluxStore, err := memory.OpenLocal(filepath.Join(t.TempDir(), "fluxmem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := flux.AttachMemory(context.Background(), fluxStore); err != nil {
		t.Fatal(err)
	}
	reg, err := collections.New(hub, flux)
	if err != nil {
		t.Fatal(err)
	}

	ms, err := memory.OpenLocal(filepath.Join(t.TempDir(), "intake"))
	if err != nil {
		t.Fatal(err)
	}
	st := intake.New(ms)
	confirmed := "---\nid: intake/it1\ntitle: Rotate\nstatus: unverified\nverified:\n  - {by: agent:validator:one, at: 2026-09-18T12:00:00Z}\n  - {by: agent:validator:two, at: 2026-09-18T12:00:00Z}\nextra:\n  intake_id: it1\n---\n# Rotate\n"
	if _, err := st.PutStaged(context.Background(), "flux", "it1", []byte(confirmed)); err != nil {
		t.Fatal(err)
	}
	unconfirmed := "---\nid: intake/it2\ntitle: Half\nverified:\n  - {by: agent:validator:one}\n---\n# Half\n"
	if _, err := st.PutStaged(context.Background(), "flux", "it2", []byte(unconfirmed)); err != nil {
		t.Fatal(err)
	}
	if err := st.Park(context.Background(), "it3", "validators disagreed"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	log, err := traversal.Open(context.Background(), &traversal.Config{Backend: "local", Path: t.TempDir(), HMACKeyEnv: "MEERKAT_TEST_PATH_KEY"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := log.Record(context.Background(), traversal.Entry{Session: "s", Outcome: "gave_up", Attempted: []string{"hub", "flux"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Record(context.Background(), traversal.Entry{Session: "s", Outcome: "found", Attempted: []string{"flux"}}); err != nil {
		t.Fatal(err)
	}
	return reg, st, log
}

func TestLibrarian_ReportsWithoutModifying(t *testing.T) {
	ctx := context.Background()
	reg, st, log := librarianFixture(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rep, err := Librarian(ctx, reg, st, LibrarianOpts{Log: log, Days: 2, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(FindingDangling) != 1 || rep.Count(FindingStale) != 1 || rep.Count(FindingCull) != 1 || rep.Count(FindingMissingLink) != 2 || rep.Count(FindingNeedsHuman) != 1 {
		var b bytes.Buffer
		rep.Write(&b)
		t.Fatalf("counts: dangling %d stale %d cull %d missing %d human %d\n%s", rep.Count(FindingDangling), rep.Count(FindingStale), rep.Count(FindingCull), rep.Count(FindingMissingLink), rep.Count(FindingNeedsHuman), b.String())
	}
	if len(rep.Fileable) != 1 || rep.Fileable[0].IntakeID != "it1" || rep.Fileable[0].Method != contentsource.UpdateDirect {
		t.Errorf("fileable = %+v", rep.Fileable)
	}
	var b bytes.Buffer
	rep.Write(&b)
	if !strings.Contains(b.String(), "hub:old") || !strings.Contains(b.String(), "archiving") || !strings.Contains(b.String(), "3 sessions searched here and gave up") {
		t.Errorf("report text:\n%s", b.String())
	}
	// Nothing was modified.
	flux, _ := reg.Get("flux")
	recs, _ := flux.Memory().Load(ctx)
	if len(recs) != 0 {
		t.Error("the report must not file anything")
	}

	applied, err := Apply(ctx, reg, st, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Action != "filed" {
		t.Fatalf("applied = %+v", applied)
	}
	recs, _ = flux.Memory().Load(ctx)
	if len(recs) != 1 || recs[0].Key != "global/intake/it1.md" {
		t.Errorf("filed = %+v", recs)
	}
	// Filing is idempotent: the next report no longer lists it.
	rep2, _ := Librarian(ctx, reg, st, LibrarianOpts{Now: func() time.Time { return now }})
	if len(rep2.Fileable) != 0 {
		t.Errorf("filed candidate listed again: %+v", rep2.Fileable)
	}
}
