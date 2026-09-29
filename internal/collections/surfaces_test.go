package collections

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// surfaces_test.go pins what the part C surfaces of meerkat-mob#25 read
// from a freshness record: which states are stale, how a failed remote
// check is classified, the advisory line's bound, and that ProbeRemote
// (`mk collections status`) never pulls.

func TestFreshness_StaleStates(t *testing.T) {
	want := map[string]bool{
		FreshCurrent: false, FreshUnknown: false,
		FreshBehindDisk: true, FreshBehindRemote: true, FreshDirty: true, FreshDiverged: true,
	}
	for _, s := range FreshnessStates() {
		stale, ok := want[s]
		if !ok {
			t.Fatalf("state %q has no expectation here; decide whether it is stale", s)
		}
		if got := (Freshness{State: s}).Stale(); got != stale {
			t.Errorf("Stale(%s) = %v, want %v", s, got, stale)
		}
		if got := (Freshness{State: s}).Advisory("kb") != ""; got != stale {
			t.Errorf("Advisory for %s present = %v, want %v", s, got, stale)
		}
	}
}

func TestFreshness_RemoteProblem(t *testing.T) {
	for note, want := range map[string]string{
		noteNotGit: "config", noteDetached: "config", noteNoUpstream: "config", noteUnsafeName: "config",
		noteRemoteFailed: "network",
		"":               "", noteDirty: "", noteDiverged: "", noteFetchedAhead: "", noteCapped: "",
	} {
		if got := (Freshness{State: FreshUnknown, Note: note}).RemoteProblem(); got != want {
			t.Errorf("RemoteProblem(note %q) = %q, want %q", note, got, want)
		}
	}
}

// Whatever the name, the advisory is one line of valid UTF-8 within
// MaxAdvisory bytes. Names are already bounded at config time; this is
// the second fence.
func TestFreshness_AdvisoryIsBoundedAndOneLine(t *testing.T) {
	// Both parities of a two-byte rune, so one of them is cut mid-rune.
	for _, name := range []string{"kb", strings.Repeat("ä", 400), "x" + strings.Repeat("ä", 400), "a\nb\rc d" + strings.Repeat("x", 300)} {
		got := (Freshness{State: FreshBehindRemote}).Advisory(name)
		if len(got) > MaxAdvisory || !utf8.ValidString(got) || strings.ContainsAny(got, "\n\r ") {
			t.Errorf("Advisory(%.20q...) = %d bytes %q; want one valid line of at most %d", name, len(got), got, MaxAdvisory)
		}
		if !strings.HasPrefix(got, "freshness: collection ") {
			t.Errorf("Advisory(%.20q...) lost its fixed prefix: %q", name, got)
		}
	}
}

// ProbeRemote reports and never acts, whatever on_divergence says: the
// tree, HEAD and the index are exactly as they were. CheckRemote on the
// same fixture then pulls, so the probe's restraint is its own.
func TestRemote_ProbeNeverPulls(t *testing.T) {
	f := newRemoteFixture(t, refresh.DivergencePull)
	head := gitT(t, f.dir, "rev-parse", "HEAD")
	tip := f.pushPage(t, "notes/zebrafish", "About zebrafish.")
	before := snapshotTree(t, f.dir)

	if _, err := f.kb.ProbeRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	fr, _ := f.kb.Freshness()
	if fr.State != FreshBehindRemote || fr.Remote != tip {
		t.Errorf("after probe: %+v, want behind-remote at %s", fr, tip)
	}
	if got := gitT(t, f.dir, "rev-parse", "HEAD"); got != head || !sameTree(before, snapshotTree(t, f.dir)) {
		t.Errorf("ProbeRemote moved the working tree (HEAD %s, was %s)", got, head)
	}
	if got := registrySearchIDs(t, f.reg, "zebrafish"); len(got) != 0 {
		t.Errorf("ProbeRemote rebuilt the index: %v", got)
	}

	if _, err := f.kb.CheckRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, f.dir, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("control: CheckRemote with on_divergence: pull did not pull (HEAD %s)", got)
	}
}
