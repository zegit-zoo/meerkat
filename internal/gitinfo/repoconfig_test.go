package gitinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoconfig_test.go proves the in-repository git calls refuse a
// repository whose own config could make them run a program
// (meerkat-mob#31), and that the strict scanner sees every key git sees.

// configCases are config files the scanner must judge. refuse is the
// verdict; the git-differential test below checks each verdict against
// git's own reading of the same file wherever git can read it.
var configCases = []struct {
	name   string
	text   string
	refuse bool
}{
	{name: "a fresh clone", text: "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n" +
		"[remote \"origin\"]\n\turl = https://example.com/kb.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n" +
		"[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n"},
	{name: "a hooksPath and fsmonitor (pinned on the command line)", text: "[core]\n\thooksPath = .husky\n\tfsmonitor = true\n"},
	{name: "a value continued onto a line that looks like a key", text: "[remote \"origin\"]\n\turl = https://example.com/kb.git\n[branch \"main\"]\n\tdescription = one \\\n[core] sshCommand = touch\n"},
	{name: "a quoted value continued", text: "[branch \"main\"]\n\tdescription = \"a ; \\\n[core] sshCommand = x\"\n"},
	{name: "a BOM and CRLF", text: "\ufeff[core]\r\n\tbare = false\r\n"},

	{name: "core.sshCommand", text: "[core]\n\tsshCommand = touch /tmp/x\n", refuse: true},
	{name: "core.sshcommand, odd case", text: "[CoRe]\n\tSSHCOMMAND = x\n", refuse: true},
	{name: "core.gitProxy", text: "[core]\n\tgitProxy = x\n", refuse: true},
	{name: "core.worktree", text: "[core]\n\tworktree = /elsewhere\n", refuse: true},
	{name: "core.askPass", text: "[core]\n\taskPass = x\n", refuse: true},
	{name: "core.alternateRefsCommand", text: "[core]\n\talternateRefsCommand = x\n", refuse: true},
	{name: "credential.helper", text: "[credential]\n\thelper = !x\n", refuse: true},
	{name: "credential.<url>.helper", text: "[credential \"https://example.com\"]\n\thelper = !x\n", refuse: true},
	{name: "a filter driver", text: "[filter \"x\"]\n\tclean = x\n\tsmudge = x\n", refuse: true},
	{name: "a filter process", text: "[filter \"x\"]\n\tprocess = x\n", refuse: true},
	{name: "a legacy filter header", text: "[filter.x]\n\tclean = x\n", refuse: true},
	{name: "include.path", text: "[include]\n\tpath = other\n", refuse: true},
	{name: "includeIf", text: "[includeIf \"gitdir:/\"]\n\tpath = other\n", refuse: true},
	{name: "url.insteadOf", text: "[url \"https://elsewhere/\"]\n\tinsteadOf = https://example.com/\n", refuse: true},
	{name: "diff textconv", text: "[diff \"x\"]\n\ttextconv = x\n", refuse: true},
	{name: "diff.external", text: "[diff]\n\texternal = x\n", refuse: true},
	{name: "a merge driver", text: "[merge \"x\"]\n\tdriver = x\n", refuse: true},
	{name: "remote.<name>.vcs", text: "[remote \"origin\"]\n\tvcs = x\n", refuse: true},
	{name: "remote.<name>.uploadpack", text: "[remote \"origin\"]\n\tuploadpack = x\n", refuse: true},
	{name: "gpg.program", text: "[gpg]\n\tprogram = x\n", refuse: true},
	{name: "a key on its header's line", text: "[core] sshCommand = x\n", refuse: true},
	{name: "a key after a value ending in an escaped space", text: "[branch \"main\"]\n\tdescription = a\\ \n[core]\n\tsshCommand = x\n", refuse: true},
	{name: "a key after a comment ending in a backslash", text: "[branch \"main\"]\n\tdescription = a ; note \\\n[core]\n\tsshCommand = x\n", refuse: true},
	{name: "a key after a comment line ending in a backslash", text: "# note \\\n[core]\n\tsshCommand = x\n", refuse: true},
	{name: "a key with no section", text: "sshCommand = x\n", refuse: true},
	{name: "an unreadable header", text: "[core\n\tsshCommand = x\n", refuse: true},
	{name: "an unreadable line", text: "[core]\n\t\"sshCommand\" = x\n", refuse: true},
	{name: "a NUL byte", text: "[core]\n\tbare = false\x00\n", refuse: true},
}

func TestCheckConfigText(t *testing.T) {
	for _, tc := range configCases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkConfigText(tc.text)
			if tc.refuse && !errors.Is(err, ErrUnsafeConfig) {
				t.Errorf("checkConfigText = %v, want ErrUnsafeConfig", err)
			}
			if !tc.refuse && err != nil {
				t.Errorf("checkConfigText = %v, want nil", err)
			}
		})
	}
}

// TestCheckConfigText_AgreesWithGit reads every case with git's own parser
// (`git config --file <f> --list`, which follows no include and runs
// nothing) and checks that whenever git sees a denied key, the scanner
// refuses — so syntax git accepts cannot hide a key from the scanner. The
// converse need not hold: the scanner also refuses text it cannot
// classify, which git may still read.
func TestCheckConfigText_AgreesWithGit(t *testing.T) {
	requireGit(t)
	for _, tc := range configCases {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "config")
			writeFile(t, f, tc.text)
			cmd := exec.Command("git", "config", "--file", f, "--list", "--name-only")
			cmd.Dir = t.TempDir()
			cmd.Env = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "PATH=" + os.Getenv("PATH")}
			out, err := cmd.Output()
			if err != nil {
				// git cannot read it either, so git would not run in it.
				return
			}
			gitDenies := false
			for _, name := range strings.Fields(string(out)) {
				parts := strings.Split(strings.ToLower(name), ".")
				if deniedConfigKey(parts[0], parts[len(parts)-1]) {
					gitDenies = true
				}
			}
			if gitDenies && !errors.Is(checkConfigText(tc.text), ErrUnsafeConfig) {
				t.Errorf("git reads a denied key (%s) the scanner let through", strings.TrimSpace(string(out)))
			}
		})
	}
}

// filterRepo is a repository whose own .git/config defines a filter
// driver and whose committed .gitattributes applies it to every page:
// any git call that reads the tree (status, checkout) would run the
// driver. The driver's command creates marker.
func filterRepo(t *testing.T, marker string) string {
	t.Helper()
	bare, _ := bareWithBranch(t, "v1")
	kb := cloneOf(t, bare)
	writeFile(t, filepath.Join(kb, ".gitattributes"), "* filter=meerkattest\n")
	git(t, kb, "add", ".gitattributes")
	git(t, kb, "commit", "-q", "-m", "attributes")
	cmd := "sh -c 'touch " + marker + "; cat'"
	git(t, kb, "config", "filter.meerkattest.clean", cmd)
	git(t, kb, "config", "filter.meerkattest.smudge", cmd)
	// A tracked file whose stat no longer matches the index, so status
	// has to re-read it through the clean filter.
	writeFile(t, filepath.Join(kb, "f.md"), "v1-edited\n")
	pushNewCommit(t, bare, "g.md", "v2")
	return kb
}

// The acceptance test of meerkat-mob#31: a repository with a hostile
// filter driver cannot execute anything through a refresh. Both in-repo
// calls refuse it before git runs.
func TestInRepoCalls_RefuseAFilterDriverAndRunNothing(t *testing.T) {
	requireGit(t)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	kb := filterRepo(t, marker)
	ctx := context.Background()

	if _, err := testRunner().Clean(ctx, kb); !errors.Is(err, ErrUnsafeConfig) {
		t.Errorf("Clean = %v, want ErrUnsafeConfig", err)
	}
	if err := testRunner().PullFFOnly(ctx, kb, "origin", "main"); !errors.Is(err, ErrUnsafeConfig) {
		t.Errorf("PullFFOnly = %v, want ErrUnsafeConfig", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repository's filter driver ran")
	}
}

// The fixture above is live: plain git, run in it, does execute the
// filter. OPT-IN (MEERKAT_TEST_EXEC=1), like TestPullFFOnly_RunsNoHooks,
// because proving it means letting git run a planted command.
func TestInRepoCalls_FilterFixtureIsLive(t *testing.T) {
	if os.Getenv("MEERKAT_TEST_EXEC") != "1" {
		t.Skip("lets git run a planted filter command; set MEERKAT_TEST_EXEC=1 to run")
	}
	requireGit(t)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	kb := filterRepo(t, marker)
	git(t, kb, "status", "--porcelain")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("plain git status did not run the filter: the refusal test proves nothing")
	}
}

// A linked worktree's own config.worktree is vetted as well as the
// shared config.
func TestCheckRepoConfig_ReadsConfigWorktree(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	if err := CheckRepoConfig(dir); err != nil {
		t.Fatalf("a plain repository: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".git", "config.worktree"), "[core]\n\tsshCommand = x\n")
	if err := CheckRepoConfig(dir); !errors.Is(err, ErrUnsafeConfig) {
		t.Errorf("config.worktree with core.sshCommand: %v, want ErrUnsafeConfig", err)
	}
}

// In-repository calls read no global config: a global config git cannot
// even parse does not reach them.
func TestInRepoCalls_ReadNoGlobalConfig(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".gitconfig"), "[[[ not a config\n")
	t.Setenv("HOME", home)
	if ok, err := testRunner().Clean(context.Background(), dir); !ok || err != nil {
		t.Fatalf("Clean = %v, %v: the global config was read", ok, err)
	}
	env := testRunner().inRepoEnv()
	for _, want := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh", "GIT_PROXY_COMMAND="} {
		if !slices.Contains(env, want) {
			t.Errorf("inRepoEnv lacks %s", want)
		}
	}
	args := testRunner().hardened("status")
	for _, want := range []string{"core.sshCommand=ssh", "credential.helper=", "core.gitProxy=", "core.hooksPath=/dev/null", "core.fsmonitor=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("hardened lacks -c %s", want)
		}
	}
}
