package ingest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// rewrite_root_test.go pins issue #91: FinalizeRewrites (and Snapshot)
// read and restore the page through an os.Root, and a page the run
// replaced with a link is rejected and restored — never written through.
// It mirrors TestFinalize_SymlinkOutOfWorkingCopyIsRefused, which pins
// the same property for Finalize (#74).

// victimPage is a parseable page that must never be touched: it differs
// from the snapshot, so a finalizer that read it through a link would
// see a changed body and "restore" the snapshot INTO it.
const victimPage = "---\nid: secret\ntitle: Not Yours\ntype: Runbook\n---\n# Secret\n"

// plannedHintRewrite plans the single hint rewrite of wiki/datadog.md
// and snapshots it, as a real run does before the agent starts.
func plannedHintRewrite(t *testing.T, dir string) ([]Task, map[string][]byte) {
	t.Helper()
	rep := rewriteReport()
	rep.PromptQuality = rep.PromptQuality[:1]
	tasks, _, err := PlanRewrites(rep, RewritePlanOpts{Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].PagePath != "wiki/datadog.md" {
		t.Fatalf("tasks = %+v", tasks)
	}
	before, err := Snapshot(dir, tasks)
	if err != nil {
		t.Fatal(err)
	}
	return tasks, before
}

func writeVictim(t *testing.T, dir string) string {
	t.Helper()
	victim := filepath.Join(dir, "victim.md")
	if err := os.WriteFile(victim, []byte(victimPage), 0o600); err != nil {
		t.Fatal(err)
	}
	return victim
}

func TestFinalizeRewrites_ReplacedPageIsRejectedNotWrittenThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating links needs a privilege we can't assume on windows")
	}
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, page, dir string) (victim string)
	}{
		{
			name: "a symlink out of the working copy",
			plant: func(t *testing.T, page, _ string) string {
				victim := writeVictim(t, t.TempDir())
				mustReplace(t, page, func() error { return os.Symlink(victim, page) })
				return victim
			},
		},
		{
			name: "a symlink to another page in the working copy",
			plant: func(t *testing.T, page, dir string) string {
				other := filepath.Join(dir, "wiki", "concepts", "drift.md")
				if err := os.WriteFile(other, []byte(victimPage), 0o600); err != nil {
					t.Fatal(err)
				}
				mustReplace(t, page, func() error { return os.Symlink(other, page) })
				return other
			},
		},
		{
			name: "a hard link to a file outside the working copy",
			plant: func(t *testing.T, page, dir string) string {
				// Same filesystem as the working copy, or link(2) refuses.
				victim := writeVictim(t, filepath.Dir(dir))
				t.Cleanup(func() { _ = os.Remove(victim) })
				mustReplace(t, page, func() error { return os.Link(victim, page) })
				return victim
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := rewriteWorkdir(t)
			tasks, before := plannedHintRewrite(t, dir)
			page := filepath.Join(dir, "wiki", "datadog.md")
			victim := tc.plant(t, page, dir)

			done, err := FinalizeRewrites(ctx, dir, []Result{{Task: tasks[0], ExitStatus: "ok"}}, before)
			if err != nil {
				t.Fatalf("FinalizeRewrites errored instead of rejecting the one item: %v", err)
			}
			if len(done) != 1 || done[0].Action != "rejected" || !strings.Contains(done[0].Detail, "replaced") {
				t.Fatalf("done = %+v; want one rejected item saying the page was replaced", done)
			}
			if got, _ := os.ReadFile(victim); string(got) != victimPage {
				t.Errorf("the link target was written through:\n%s", got)
			}
			fi, err := os.Lstat(page)
			if err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("page is not a regular file after restore: %v %v", fi, err)
			}
			if vfi, err := os.Stat(victim); err == nil && os.SameFile(fi, vfi) {
				t.Error("the restored page still shares an inode with the link target")
			}
			if got, _ := os.ReadFile(page); string(got) != string(before["wiki/datadog.md"]) {
				t.Errorf("page not restored to its snapshot:\n%s", got)
			}
			// Nothing the link pointed at reaches the report.
			if strings.Contains(done[0].Before+done[0].After, "Not Yours") {
				t.Errorf("report carries the link target's content: %+v", done[0])
			}
		})
	}
}

// A path component that now leads out of the working copy cannot even
// be restored through: the item fails, the batch carries on, and the
// outside file is untouched.
func TestFinalizeRewrites_DirectoryLinkedOutIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege we can't assume on windows")
	}
	ctx := context.Background()
	dir := rewriteWorkdir(t)
	tasks, before := plannedHintRewrite(t, dir)

	outside := t.TempDir()
	victim := filepath.Join(outside, "datadog.md")
	if err := os.WriteFile(victim, []byte(victimPage), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "wiki")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "wiki")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	// Precondition: the lexical pre-check sees nothing wrong — that is
	// why os.Root has to be the gate behind it.
	if !isPathWithinBase(dir, filepath.Join(dir, "wiki", "datadog.md")) {
		t.Fatal("precondition: isPathWithinBase is expected to accept the symlinked path")
	}

	done, err := FinalizeRewrites(ctx, dir, []Result{{Task: tasks[0], ExitStatus: "ok"}}, before)
	if err != nil {
		t.Fatalf("FinalizeRewrites errored instead of failing the one item: %v", err)
	}
	if len(done) != 1 || done[0].Action != "failed" || !strings.Contains(done[0].Detail, "escapes the working copy") {
		t.Fatalf("done = %+v; want one failed item naming the working-copy escape", done)
	}
	if got, _ := os.ReadFile(victim); string(got) != victimPage {
		t.Errorf("a file outside the working copy was rewritten:\n%s", got)
	}
}

// Snapshot refuses a page that is already a link, before any agent runs.
func TestSnapshot_RefusesALinkedPage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege we can't assume on windows")
	}
	dir := rewriteWorkdir(t)
	rep := rewriteReport()
	rep.PromptQuality = rep.PromptQuality[:1]
	tasks, _, err := PlanRewrites(rep, RewritePlanOpts{Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	victim := writeVictim(t, t.TempDir())
	page := filepath.Join(dir, "wiki", "datadog.md")
	mustReplace(t, page, func() error { return os.Symlink(victim, page) })

	if _, err := Snapshot(dir, tasks); err == nil {
		t.Fatal("Snapshot read a page that is a symlink out of the working copy")
	}
}

// mustReplace removes path and plants a link in its place, skipping the
// test where the platform refuses the link.
func mustReplace(t *testing.T, path string, link func() error) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := link(); err != nil {
		t.Skipf("links unavailable here: %v", err)
	}
}
