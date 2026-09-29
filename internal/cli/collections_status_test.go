package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/collections"
)

// collections_status_test.go drives `mk collections status` through the
// real command tree and a real content-source.yaml (meerkat-mob#25 part
// C, MK-FRESH-08). Remotes are bare repositories on disk.

func gitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// clonedKB returns a working-tree clone of a fresh bare remote holding
// one page, and the bare remote's path.
func clonedKB(t *testing.T) (dir, bare string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Cleanup(collections.UseFileRemotesForTest())
	seed := newNamedKBDir(t, "kb")
	gitCLI(t, seed, "init", "-q")
	gitCLI(t, seed, "add", ".")
	gitCLI(t, seed, "commit", "-q", "-m", "seed")
	bare = filepath.Join(t.TempDir(), "kb.git")
	gitCLI(t, seed, "clone", "-q", "--bare", seed, bare)
	dir = filepath.Join(t.TempDir(), "kb")
	gitCLI(t, t.TempDir(), "clone", "-q", bare, dir)
	return dir, bare
}

// pushAhead commits one page to bare from a scratch clone and returns
// the new tip.
func pushAhead(t *testing.T, bare string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "pusher")
	gitCLI(t, t.TempDir(), "clone", "-q", bare, wt)
	write(t, filepath.Join(wt, "wiki", "kb", "ahead.md"), "---\nid: kb/ahead\ntitle: Ahead\n---\n# Ahead\n")
	gitCLI(t, wt, "add", ".")
	gitCLI(t, wt, "commit", "-q", "-m", "ahead")
	gitCLI(t, wt, "push", "-q", "origin", "main")
	return gitCLI(t, wt, "rev-parse", "HEAD")
}

// statusConfig mounts dir as "kb" with the given refresh: block (empty
// for none) and a plain "other" collection.
func statusConfig(t *testing.T, dir, refreshBlock string) string {
	t.Helper()
	other := newNamedKBDir(t, "other")
	path := filepath.Join(t.TempDir(), "content-source.yaml")
	body := "collections:\n  - name: kb\n    type: local\n    path: " + dir + "\n"
	if refreshBlock != "" {
		body += "    refresh:\n" + refreshBlock
	}
	body += "  - name: other\n    type: local\n    path: " + other + "\n"
	write(t, path, body)
	return path
}

const remoteRefresh = "      interval: 1m\n      remote_check: 1m\n"

func TestCollectionsStatus_BehindRemoteFailsCheck(t *testing.T) {
	resetKBToEmbedded(t)
	dir, bare := clonedKB(t)
	tip := pushAhead(t, bare)
	cfg := statusConfig(t, dir, remoteRefresh)

	out, err := execRoot(t, "--content-source", cfg, "collections", "status")
	if err != nil {
		t.Fatalf("status without --check must succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "behind-remote") || !strings.Contains(out, tip[:12]) || strings.Contains(out, "other") {
		t.Errorf("table = %q, want kb behind-remote at %s and no row for the unconfigured collection", out, tip[:12])
	}

	out, err = execRoot(t, "--content-source", cfg, "collections", "status", "--check")
	if err == nil || !strings.Contains(err.Error(), "kb is behind-remote") {
		t.Errorf("--check on a behind-remote collection: err = %v, want a failure naming it\n%s", err, out)
	}
}

func TestCollectionsStatus_JSON(t *testing.T) {
	resetKBToEmbedded(t)
	dir, bare := clonedKB(t)
	tip := pushAhead(t, bare)
	out, err := execRoot(t, "--content-source", statusConfig(t, dir, remoteRefresh), "collections", "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	var rows []statusRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].Collection != "kb" || rows[0].Freshness.State != collections.FreshBehindRemote || rows[0].Freshness.Remote != tip {
		t.Errorf("rows = %+v, want kb behind-remote at %s only", rows, tip)
	}
}

func TestCollectionsStatus_CurrentPassesCheck(t *testing.T) {
	resetKBToEmbedded(t)
	dir, _ := clonedKB(t)
	out, err := execRoot(t, "--content-source", statusConfig(t, dir, remoteRefresh), "collections", "status", "--check")
	if err != nil || !strings.Contains(out, "current") {
		t.Errorf("--check on a current collection: err = %v\n%s", err, out)
	}
}

// The command reports and never acts: a pull policy is not applied.
func TestCollectionsStatus_NeverPulls(t *testing.T) {
	resetKBToEmbedded(t)
	dir, bare := clonedKB(t)
	head := gitCLI(t, dir, "rev-parse", "HEAD")
	pushAhead(t, bare)
	cfg := statusConfig(t, dir, remoteRefresh+"      on_divergence: pull\n")
	if _, err := execRoot(t, "--content-source", cfg, "collections", "status"); err != nil {
		t.Fatal(err)
	}
	if got := gitCLI(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("mk collections status pulled: HEAD %s, was %s", got, head)
	}
}

// A check the checkout's own configuration stops fails --check; an
// unreachable remote does not. Both say which.
func TestCollectionsStatus_ConfigProblemFailsNetworkProblemDoesNot(t *testing.T) {
	resetKBToEmbedded(t)
	notGit := newNamedKBDir(t, "kb")
	out, err := execRoot(t, "--content-source", statusConfig(t, notGit, remoteRefresh), "collections", "status", "--check")
	if err == nil || !strings.Contains(err.Error(), "remote check could not run") || !strings.Contains(out, "config problem") {
		t.Errorf("not a git tree: err = %v, want a config failure\n%s", err, out)
	}

	dir, bare := clonedKB(t)
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	out, err = execRoot(t, "--content-source", statusConfig(t, dir, remoteRefresh), "collections", "status", "--check")
	if err != nil || !strings.Contains(out, "network problem") || !strings.Contains(out, "unknown") {
		t.Errorf("unreachable remote: err = %v, want exit 0 reporting a network problem\n%s", err, out)
	}
}

func TestCollectionsStatus_NothingConfigured(t *testing.T) {
	resetKBToEmbedded(t)
	out, err := execRoot(t, "--content-source", statusConfig(t, newNamedKBDir(t, "kb"), ""), "collections", "status", "--check")
	if err != nil || !strings.Contains(out, "nothing to report") {
		t.Errorf("no refresh: block: err = %v\n%s", err, out)
	}
}

// `mk version` names the commit a refreshed local collection is checked
// out at, read from git's files: no network, so an unreachable remote
// changes nothing. Other collections carry no commit.
func TestVersion_ReportsLocalCommitOffline(t *testing.T) {
	resetKBToEmbedded(t)
	dir, bare := clonedKB(t)
	head := gitCLI(t, dir, "rev-parse", "HEAD")
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	out, err := execRoot(t, "--content-source", statusConfig(t, dir, remoteRefresh), "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\n%s", err, out)
	}
	var info versionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, c := range info.Collections {
		got[c.Name] = c.Commit
	}
	if got["kb"] != head || got["other"] != "" {
		t.Errorf("commits = %v, want kb at %s and none for other", got, head)
	}
}
