package intake

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Every key the store builds from a caller-influenced value stays inside
// its stage: "." and ".." and anything with a separator are refused
// rather than cleaned by path.Join (meerkat-mob#38).
func TestKeys_RefuseUnsafeSegments(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	bad := []string{"", ".", "..", "a/b", "../x", `a\b`, "a\nb", strings.Repeat("x", maxSegment+1)}
	for _, s := range bad {
		if err := Segment(s); err == nil {
			t.Errorf("Segment(%q) accepted", s)
		}
		if k, err := StagedKey(s, "id1"); err == nil {
			t.Errorf("StagedKey(%q, id1) = %q", s, k)
		}
		if k, err := StagedKey("flux", s); err == nil {
			t.Errorf("StagedKey(flux, %q) = %q", s, k)
		}
		if _, err := st.PutStaged(ctx, s, "id1", []byte("x")); err == nil {
			t.Errorf("PutStaged(%q) accepted", s)
		}
		if err := st.MarkDone(ctx, s, "n"); err == nil {
			t.Errorf("MarkDone(%q) accepted", s)
		}
		if _, err := st.IsDone(ctx, s); err == nil {
			t.Errorf("IsDone(%q) accepted", s)
		}
		if err := st.Park(ctx, s, "r"); err == nil {
			t.Errorf("Park(%q) accepted", s)
		}
		if err := st.Unpark(ctx, s); err == nil {
			t.Errorf("Unpark(%q) accepted", s)
		}
		if s != "" {
			if _, err := st.PutRaw(ctx, s, time.Now(), "id1", []byte("x")); err == nil {
				t.Errorf("PutRaw(namespace %q) accepted", s)
			}
		}
		if _, err := st.PutRaw(ctx, "ns", time.Now(), s, []byte("x")); err == nil {
			t.Errorf("PutRaw(id %q) accepted", s)
		}
	}
	recs, err := st.s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("refused keys wrote %d objects: %+v", len(recs), recs)
	}
	for _, ok := range []string{"flux", "validated-0123abcd", "My Collection.v2", "anonymous"} {
		if err := Segment(ok); err != nil {
			t.Errorf("Segment(%q) = %v", ok, err)
		}
	}
	if k, err := StagedKey("flux", "id1"); err != nil || k != "staged/flux/id1.md" {
		t.Errorf("StagedKey = %q %v", k, err)
	}
}
