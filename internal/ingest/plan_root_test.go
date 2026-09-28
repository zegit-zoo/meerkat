package ingest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// plan_root_test.go pins issue #92: the PLANNING half of the intake and
// librarian paths writes and reads through an os.Root, so a link left in
// a persistent working copy by an earlier run cannot redirect a planning
// write or read out of the tree. #74 and #91 did the same for the
// finalizing half.

func skipWithoutSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("creating links needs a privilege we can't assume on windows")
	}
}

// symlinkOrSkip plants a link, skipping where the platform refuses.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

// A researcher plan copies each raw deposit into ingestion/intake/. With
// that directory linked out of the working copy, the copy must fail
// rather than land outside.
func TestPlanResearch_LinkedIntakeDirCannotRedirectTheRawWrite(t *testing.T) {
	skipWithoutSymlinks(t)
	st, workdir := intakeFixture(t)
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(workdir, filepath.FromSlash(IntakeDir)))

	_, _, err := PlanIntake(context.Background(), st, IntakePlanOpts{Role: RoleResearcher, Workdir: workdir, Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "escapes the working copy") {
		t.Fatalf("PlanIntake = %v; want the raw write refused as a working-copy escape", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the raw deposit was written outside the working copy: %v", entries)
	}
}

// A link sitting AT the raw path is replaced, never written through: the
// target outside is untouched and the raw path becomes a regular file.
func TestPlanResearch_LinkAtTheRawPathIsReplaced(t *testing.T) {
	skipWithoutSymlinks(t)
	st, workdir := intakeFixture(t)
	victim := writeVictim(t, t.TempDir())
	rawPath := filepath.Join(workdir, filepath.FromSlash(IntakeDir), "it1.md")
	symlinkOrSkip(t, victim, rawPath)

	tasks, _, err := PlanIntake(context.Background(), st, IntakePlanOpts{Role: RoleResearcher, Workdir: workdir, Model: "m"})
	if err != nil {
		t.Fatalf("PlanIntake: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if got, _ := os.ReadFile(victim); string(got) != victimPage {
		t.Errorf("the link target was written through:\n%s", got)
	}
	fi, err := os.Lstat(rawPath)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("raw path is not a regular file after planning: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(rawPath); !strings.Contains(string(b), "Organization Settings") {
		t.Errorf("raw path does not hold the deposit:\n%s", b)
	}
}

// A validator plan reads the candidate it validates. A candidate that is a
// link out of the working copy is skipped, and nothing it points at reaches
// a prompt.
func TestPlanValidation_LinkedCandidateIsSkippedNotRead(t *testing.T) {
	skipWithoutSymlinks(t)
	ctx := context.Background()
	st, workdir := intakeFixture(t)
	body := []byte("---\nid: intake/it9\ntitle: Staged\ntype: concept\nstatus: unverified\n---\n# Staged\n\nA staged candidate.\n")
	if _, err := st.PutStaged(ctx, "flux", "it9", body); err != nil {
		t.Fatal(err)
	}
	victim := writeVictim(t, t.TempDir())
	symlinkOrSkip(t, victim, filepath.Join(workdir, "wiki", "intake", "it9.md"))

	tasks, skips, err := PlanIntake(ctx, st, IntakePlanOpts{Role: RoleValidator, Workdir: workdir, Model: "validator-model"})
	if err != nil {
		t.Fatalf("PlanIntake: %v", err)
	}
	for _, task := range tasks {
		if task.IntakeID == "it9" {
			t.Fatalf("a candidate that links out of the working copy was planned: %+v", task)
		}
		if strings.Contains(task.Prompt, "Not Yours") {
			t.Fatalf("the link target's content reached a prompt: %s", task.Prompt)
		}
	}
	found := false
	for _, s := range skips {
		if s.IntakeID == "it9" && strings.Contains(s.Reason, "escapes the working copy") {
			found = true
		}
	}
	if !found {
		t.Errorf("skips = %+v; want it9 skipped as a working-copy escape", skips)
	}
	if got, _ := os.ReadFile(victim); string(got) != victimPage {
		t.Errorf("the link target was modified:\n%s", got)
	}
}

// With the candidates' directory linked out of the working copy, the seed
// write for a missing candidate is not followed out either.
func TestPlanValidation_LinkedCandidateDirIsRefused(t *testing.T) {
	skipWithoutSymlinks(t)
	ctx := context.Background()
	st, workdir := intakeFixture(t)
	body := []byte("---\nid: intake/it9\ntitle: Staged\ntype: concept\nstatus: unverified\n---\n# Staged\n\nA staged candidate.\n")
	if _, err := st.PutStaged(ctx, "flux", "it9", body); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(workdir, "wiki", "intake"))

	tasks, skips, err := PlanIntake(ctx, st, IntakePlanOpts{Role: RoleValidator, Workdir: workdir, Model: "validator-model"})
	if err != nil {
		t.Fatalf("PlanIntake: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("planned %+v through a linked directory", tasks)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "escapes the working copy") {
		t.Errorf("skips = %+v; want one working-copy escape", skips)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the seed was written outside the working copy: %v", entries)
	}
}

// The tool-description proposal is written through the same helper.
func TestWriteToolProposal_LinkedProposalDirIsRefused(t *testing.T) {
	skipWithoutSymlinks(t)
	workdir := t.TempDir()
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(workdir, filepath.FromSlash(ToolProposalDir)))

	if _, err := WriteToolProposal(rewriteReport(), workdir, time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("WriteToolProposal wrote through a directory linked out of the working copy")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the proposal was written outside the working copy: %v", entries)
	}
}
