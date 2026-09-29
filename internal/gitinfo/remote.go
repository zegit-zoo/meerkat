package gitinfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// remote.go is the ONLY place meerkat runs git for a knowledge base
// (MK-SEC-12 as amended by meerkat-mob#26: "the probe never invokes git;
// the only git calls are the opt-in pull path (git status --porcelain,
// git pull --ff-only) and the opt-in remote_check (git ls-remote)").
// Each call has a fixed argument vector and no shell, runs under an
// environment built from an allowlist, cannot prompt, and has a timeout.

// ErrNotFastForward is a pull that could not fast-forward: local and
// remote have diverged.
var ErrNotFastForward = errors.New("not a fast-forward: local and remote have diverged")

// Runner runs the hardened git commands. The zero value is not usable;
// DefaultRunner is.
type Runner struct {
	// Git is the git executable, looked up on PATH when bare.
	Git string
	// AllowProtocols is GIT_ALLOW_PROTOCOL: the transports git may use.
	// It exists because a remote URL comes from repository config, and
	// git's `ext::` transport would otherwise make that URL a command.
	AllowProtocols string
	// RemoteTimeout bounds ls-remote; PullTimeout bounds status and pull.
	RemoteTimeout time.Duration
	PullTimeout   time.Duration
}

// DefaultRunner is what meerkat uses.
var DefaultRunner = Runner{
	Git:            "git",
	AllowProtocols: "https:ssh:git",
	RemoteTimeout:  30 * time.Second,
	PullTimeout:    2 * time.Minute,
}

// env is the whole environment a git call gets: an allowlist of the
// caller's (so the user's own ssh agent and global config can still
// authenticate a private remote) plus fixed settings that forbid
// prompting and pin the transports.
func (r Runner) env() []string {
	env := []string{
		"LC_ALL=C", // stable messages: ErrNotFastForward matches on one
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=" + r.AllowProtocols,
	}
	for _, k := range []string{"PATH", "HOME", "SSH_AUTH_SOCK"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// LsRemote returns the commit the remote's branch points at.
//
// It runs OUTSIDE the knowledge base: in an empty temporary directory,
// with repository discovery stopped there, and with the URL passed as an
// argument after `--`. So nothing in the knowledge base's own
// `.git/config` applies to it (no core.sshCommand, no credential.helper,
// no url.*.insteadOf rewrite), and a URL starting with `-` cannot be read
// as an option.
func (r Runner) LsRemote(ctx context.Context, url, branch string) (string, error) {
	if url == "" || branch == "" {
		return "", errors.New("ls-remote: empty url or branch")
	}
	scratch, err := os.MkdirTemp("", "meerkat-ls-remote-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	ctx, cancel := context.WithTimeout(ctx, r.RemoteTimeout)
	defer cancel()
	ref := "refs/heads/" + branch
	cmd := exec.CommandContext(ctx, r.Git, "ls-remote", "--heads", "--", url, ref) //nolint:gosec // G204: fixed argv, no shell; url and ref come after "--", and GIT_ALLOW_PROTOCOL pins the transports.
	cmd.Dir = scratch
	cmd.Env = append(r.env(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(scratch))
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git ls-remote: %w: %s", err, firstLine(stderr.String()))
	}
	for _, line := range strings.Split(out.String(), "\n") {
		name, got, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && got == ref && isObjectName(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("git ls-remote: the remote has no branch %q", branch)
}

// hardened is the prefix every in-repository call gets: no hooks, no
// fsmonitor (which executes a configured program), no optional index
// writes. It does not neutralise the repository's own config otherwise;
// that residual is the operator's opt-in (docs/SECURITY.md).
func (r Runner) hardened(args ...string) []string {
	return append([]string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"--no-optional-locks",
	}, args...)
}

// Clean reports whether the working tree has no uncommitted changes to
// tracked files: `git status --porcelain --untracked-files=no` is empty.
// Untracked files do not count: a fast-forward refuses to overwrite one
// anyway, and a scratch file in a knowledge base must not block updates
// forever.
func (r Runner) Clean(ctx context.Context, root string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.PullTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Git, r.hardened("status", "--porcelain", "--untracked-files=no")...) //nolint:gosec // G204: fixed argv, no shell; nothing configurable.
	cmd.Dir = root
	cmd.Env = r.env()
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("git status: %w: %s", err, firstLine(stderr.String()))
	}
	return strings.TrimSpace(out.String()) == "", nil
}

// PullFFOnly fast-forwards the checked-out branch to the remote's, and
// does nothing else: never a merge commit, a rebase, a stash, a checkout
// or a reset. A branch that cannot fast-forward returns ErrNotFastForward
// and leaves the working tree as it was.
func (r Runner) PullFFOnly(ctx context.Context, root, remote, branch string) error {
	// git pull forwards remote and branch to fetch without a `--`, so a
	// name starting with `-` would be an option there. Upstream refuses
	// such names already; this is the second line.
	if err := CheckName(remote); err != nil {
		return fmt.Errorf("git pull: remote: %w", err)
	}
	if err := CheckName(branch); err != nil {
		return fmt.Errorf("git pull: branch: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.PullTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Git, r.hardened("pull", "--ff-only", "--no-rebase", "--no-edit", "--", remote, branch)...) //nolint:gosec // G204: fixed argv, no shell; remote and branch pass CheckName (no leading "-"), read from the repo's own config.
	cmd.Dir = root
	cmd.Env = r.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "Not possible to fast-forward") {
			return ErrNotFastForward
		}
		return fmt.Errorf("git pull --ff-only: %w: %s", err, firstLine(stderr.String()))
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
