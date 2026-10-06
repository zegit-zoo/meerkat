package forgetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/forge"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := &Fake{}
	url, n, err := f.CreateIssue(ctx, "team/kb", "t", "<!-- m -->\nb", []string{"needs-human"})
	if err != nil || n != 1 || !strings.HasSuffix(url, "/team/kb/issues/1") {
		t.Fatalf("create = %q %d %v", url, n, err)
	}
	if st, _, _ := f.IssueState(ctx, "team/kb", 1); st != forge.StateOpen {
		t.Errorf("state = %q", st)
	}
	if u, n, ok, err := f.FindIssue(ctx, "team/kb", "<!-- m -->", time.Time{}); !ok || n != 1 || u != url || err != nil {
		t.Errorf("find = %q %d %v %v", u, n, ok, err)
	}
	if _, _, ok, _ := f.FindIssue(ctx, "team/kb", "<!-- other -->", time.Time{}); ok {
		t.Error("another marker is not found")
	}
	f.Close("team/kb", 1, "resolved")
	if st, labels, _ := f.IssueState(ctx, "team/kb", 1); st != forge.StateClosed || len(labels) != 2 {
		t.Errorf("after close: %q %v", st, labels)
	}
	if _, _, err := f.IssueState(ctx, "team/kb", 2); err == nil {
		t.Error("unknown issue")
	}
	if len(f.Issues()) != 1 {
		t.Error("issues")
	}
	f.Err = errors.New("down")
	if _, _, err := f.CreateIssue(ctx, "team/kb", "t", "b", nil); err == nil {
		t.Error("Err fails create")
	}
	if _, _, err := f.IssueState(ctx, "team/kb", 1); err == nil {
		t.Error("Err fails state")
	}
	if _, _, _, err := f.FindIssue(ctx, "team/kb", "m", time.Time{}); err == nil {
		t.Error("Err fails find")
	}
}
