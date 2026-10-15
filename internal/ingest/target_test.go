package ingest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/forge/forgetest"
	"github.com/zegit-zoo/meerkat/internal/intake"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// The target comes from what mk_report_outcome recorded, never from the
// attempted list the caller typed (meerkat-mob#38).
func TestTargetKB_OnlyTheRecordedTarget(t *testing.T) {
	cases := []struct {
		it   intake.Item
		want string
	}{
		{intake.Item{TargetKB: "flux", Attempted: []string{"victim"}}, "flux"},
		{intake.Item{Attempted: []string{"root", "victim"}}, "unrouted"},
		{intake.Item{TargetKB: ".."}, "unrouted"},
		{intake.Item{TargetKB: "a/b"}, "unrouted"},
		{intake.Item{}, "unrouted"},
	}
	for _, c := range cases {
		if got := targetKB(c.it); got != c.want {
			t.Errorf("targetKB(%+v) = %q; want %q", c.it, got, c.want)
		}
	}
}

// Apply files only into the collection the deposit was authorised for.
func TestApply_RefusesATargetTheDepositWasNotAuthorisedFor(t *testing.T) {
	ctx := context.Background()
	victim := collections.FromPages("victim", []kb.Page{{ID: "p", Title: "P"}})
	victim.Source.Update = &contentsource.UpdateSpec{Method: contentsource.UpdateDirect}
	victim.Source.Update.Normalize()
	vmem, err := memory.OpenLocal(filepath.Join(t.TempDir(), "victim"))
	if err != nil {
		t.Fatal(err)
	}
	if err := victim.AttachMemory(ctx, vmem); err != nil {
		t.Fatal(err)
	}
	reg, err := collections.New(victim)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := memory.OpenLocal(filepath.Join(t.TempDir(), "intake"))
	if err != nil {
		t.Fatal(err)
	}
	st := intake.New(ms)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	// A deposit authorised for "mine", and one from before targets were
	// recorded, both staged under "victim".
	for id, front := range map[string]string{
		"x1": `{"id":"x1","fallback_kind":"web","attempted":["victim"],"target_kb":"mine"}`,
		"x2": `{"id":"x2","fallback_kind":"web","attempted":["victim"]}`,
	} {
		if _, err := st.PutRaw(ctx, "mallory", now, id, []byte("---\n"+front+"\n---\nbody\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutStaged(ctx, "victim", id, []byte("---\nid: intake/"+id+"\ntitle: T\n---\n# T\n")); err != nil {
			t.Fatal(err)
		}
	}
	rep := &Report{Fileable: []Fileable{
		{IntakeID: "x1", Collection: "victim", Method: contentsource.UpdateDirect, Key: "staged/victim/x1.md", PageID: "intake/x1"},
		{IntakeID: "x2", Collection: "victim", Method: contentsource.UpdateDirect, Key: "staged/victim/x2.md", PageID: "intake/x2"},
	}}
	applied, err := Apply(ctx, reg, st, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %+v", applied)
	}
	for _, a := range applied {
		if a.Action != "skipped" || !strings.Contains(a.Detail, "authorised for") {
			t.Errorf("%s = %+v; want skipped as not authorised", a.IntakeID, a)
		}
	}
	if recs, _ := vmem.Load(ctx); len(recs) != 0 {
		t.Errorf("filed into a collection the deposit did not target: %+v", recs)
	}
}

// Escalation files an issue only on the forge of the authorised target.
func TestEscalate_RefusesATargetTheDepositWasNotAuthorisedFor(t *testing.T) {
	ctx := context.Background()
	t.Setenv(escalateTokenEnv, "not a secret, a test token long enough")
	reg, st := escalateFixture(t, escalateTokenEnv)
	raw := []byte("---\n{\"id\":\"ev1\",\"question\":\"q\",\"attempted\":[\"handbook\"],\"target_kb\":\"notes\"}\n---\nbody\n")
	if _, err := st.PutRaw(ctx, "mallory", time.Now(), "ev1", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutStaged(ctx, "handbook", "ev1", []byte("---\nid: intake/ev1\ntitle: E\n---\n# E\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.Park(ctx, "ev1", "needs-human: x"); err != nil {
		t.Fatal(err)
	}
	f := &forgetest.Fake{}
	applied, err := ApplyWith(ctx, reg, st, librarianRun(t, reg, st), fakeForge(f))
	if err != nil {
		t.Fatal(err)
	}
	ev := byID(applied, "ev1")
	if len(ev) != 1 || ev[0].Action != "skipped" || !strings.Contains(ev[0].Detail, `authorised for "notes"`) {
		t.Fatalf("ev1 = %+v", ev)
	}
	for _, is := range f.Issues() {
		if strings.Contains(is.Body, "ev1") {
			t.Errorf("an issue was filed for the unauthorised target: %+v", is)
		}
	}
	if hb := byID(applied, "hb1"); len(hb) != 1 || hb[0].Action != "filed" {
		t.Errorf("the authorised item must still be filed: %+v", hb)
	}
}
