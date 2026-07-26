package contentsource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/auth"
)

// Embed-target locations, relative to repoRoot.
const (
	destContent = "internal/kb/content"    // <- layout.Wiki
	destEtc     = "internal/sources/etc"   // <- sources.yaml, prompts/, templates/
	StampFile   = ".meerkat-content-stamp" // resolved content commit, for kbCommit provenance
)

// Sync resolves the configured content source and populates the embed
// directories, returning the resolved content commit (for `mk version`).
// A "none" source is a no-op that preserves the committed placeholders.
func Sync(repoRoot string, out io.Writer) (commit string, err error) {
	cfg, err := Load(repoRoot)
	if err != nil {
		return "", err
	}
	src := cfg.Content

	if src.Type == TypeNone {
		fmt.Fprintln(out, "content: type=none — leaving embedded placeholders in place")
		return writeStamp(repoRoot, "none")
	}

	if src.Type == TypeGit && MovingRef(src.Ref) {
		fmt.Fprintf(out, "content: warning: ref %q is not pinned (tag or SHA); build is not reproducible\n", src.Ref)
	}

	srcRoot, commit, err := resolve(repoRoot, src, out)
	if err != nil {
		return "", err
	}
	if err := copyTree(repoRoot, srcRoot, src.Layout, out); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "content: synced type=%s commit=%s from %s\n", src.Type, commit, srcRoot)
	return writeStamp(repoRoot, commit)
}

// WorkingCopy is a resolved content source ready for ingest write-back.
type WorkingCopy struct {
	Path    string // local dir, submodule path, or git cache checkout
	Branch  string // push target
	WikiDir string // layout.wiki, relative to Path
	Type    string
}

// ResolveWorkingCopy loads content-source.yaml and resolves the source to an
// on-disk working copy for ingestion — without copying into the embed dirs.
// Errors when the source is none/absent (ingest needs a real target).
func ResolveWorkingCopy(repoRoot string, out io.Writer) (WorkingCopy, error) {
	cfg, err := Load(repoRoot)
	if err != nil {
		return WorkingCopy{}, err
	}
	src := cfg.Content
	if src.Type == TypeNone {
		return WorkingCopy{}, fmt.Errorf("content-source.yaml is %q; set a local|git|submodule source (or pass --workdir-kb)", TypeNone)
	}
	dir, _, err := resolve(repoRoot, src, out)
	if err != nil {
		return WorkingCopy{}, err
	}
	return WorkingCopy{
		Path:    dir,
		Branch:  src.resolvedBranch(dir),
		WikiDir: src.Layout.Wiki,
		Type:    src.Type,
	}, nil
}

// resolvedBranch picks the ingest push target: explicit Branch, else Ref when
// it looks like a branch (not a pinned tag/SHA), else the remote default, else
// "main".
func (s Source) resolvedBranch(dir string) string {
	if s.Branch != "" {
		return s.Branch
	}
	if s.Ref != "" && MovingRef(s.Ref) {
		return s.Ref
	}
	if b, err := gitOut(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && b != "" {
		return strings.TrimPrefix(b, "origin/")
	}
	if b, err := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && b != "" && b != "HEAD" {
		return b
	}
	return "main"
}

func resolve(repoRoot string, src Source, out io.Writer) (root, commit string, err error) {
	switch src.Type {
	case TypeLocal:
		root = src.Path
		if !filepath.IsAbs(root) {
			root = filepath.Join(repoRoot, root)
		}
		if fi, statErr := os.Stat(root); statErr != nil || !fi.IsDir() {
			return "", "", fmt.Errorf("content.path %q is not a directory", src.Path)
		}
		return root, localCommit(filepath.Join(root, src.Layout.Wiki)), nil

	case TypeSubmodule:
		if err := runGit(out, repoRoot, "submodule", "update", "--init", src.Submodule); err != nil {
			return "", "", fmt.Errorf("submodule update %q: %w", src.Submodule, err)
		}
		root = filepath.Join(repoRoot, src.Submodule)
		sha, _ := gitOut(root, "rev-parse", "--short", "HEAD")
		return root, fallback(sha, "submodule"), nil

	case TypeGit:
		return resolveGit(src, out)
	}
	return "", "", fmt.Errorf("unsupported type %q", src.Type)
}

// resolveGit clones (or refreshes) the configured repo at ref into a
// user-owned cache dir and returns the checked-out path + short commit SHA.
// Private GitHub repos authenticate via `gh`'s cached OAuth token, handed
// to git through the environment (see authClone) rather than the clone URL
// or argv; the token is never persisted to the cache's remote config.
// GitLab (and any other host) has no credential borrowing: the clone
// proceeds with the plain tokenless URL, relying on anonymous access or
// the user's own git credential configuration (a full clone URL / SSH
// spec, a credential helper, etc.) for private access.
func resolveGit(src Source, out io.Writer) (root, commit string, err error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = filepath.Join(os.TempDir(), "meerkat-cache")
	}
	root = filepath.Join(cacheRoot, "meerkat", "content", sanitize(src.Repo))
	cleanURL := cloneURL(src) // always tokenless — safe to persist and to log
	authURL, gitArgs, gitEnv := authClone(src)

	exists := dirExists(filepath.Join(root, ".git"))
	if !exists {
		if err := os.MkdirAll(filepath.Dir(root), 0o750); err != nil {
			return "", "", err
		}
	} else {
		_ = runGit(out, root, "remote", "set-url", "origin", authURL)
	}
	// Always drop the (at most username-bearing, never token-bearing —
	// see authClone) remote URL back to the plain form before we return,
	// no matter how the clone/fetch below turns out. A `defer` here (rather
	// than a single call after the if/else) matters: without it, an error
	// return from the fetch branch below used to skip this scrub entirely,
	// leaving a live credential path in the user's cache dir indefinitely.
	defer func() { _ = runGit(out, root, "remote", "set-url", "origin", cleanURL) }()

	if !exists {
		cloneArgs := append(append([]string{}, gitArgs...), "clone", "--quiet", authURL, root)
		if err := runGitEnv(out, "", gitEnv, cloneArgs...); err != nil {
			return "", "", fmt.Errorf("clone %s: %w", redact(authURL), err)
		}
	} else {
		fetchArgs := append(append([]string{}, gitArgs...), "fetch", "--quiet", "--tags", "--force", "origin")
		if err := runGitEnv(out, root, gitEnv, fetchArgs...); err != nil {
			return "", "", fmt.Errorf("fetch: %w", err)
		}
	}

	if err := runGit(out, root, "checkout", "--quiet", "--force", "--detach", src.Ref); err != nil {
		return "", "", fmt.Errorf("checkout %q (fetch may be needed): %w", src.Ref, err)
	}
	sha, _ := gitOut(root, "rev-parse", "--short", "HEAD")
	return root, fallback(sha, "git"), nil
}

// cloneURL builds the plain (always tokenless, no embedded username either)
// HTTPS clone URL for src. A full URL (or git@ SSH form) in Repo is used
// verbatim. This is what we persist as the remote and what's safe to log.
func cloneURL(src Source) string {
	if strings.Contains(src.Repo, "://") || strings.HasPrefix(src.Repo, "git@") {
		return src.Repo
	}
	host := src.Host
	if host == "" {
		host = "github"
	}
	domain := map[string]string{"github": "github.com", "gitlab": "gitlab.com"}[host]
	return fmt.Sprintf("https://%s/%s.git", domain, src.Repo)
}

// credentialHelperScript is an inline git credential helper (the
// "!<shell>" form documented under credential.helper) that answers a `get`
// request with the token from $MEERKAT_GIT_TOKEN. Because the token is
// read back from the environment rather than substituted into the
// script text, it never appears in this string, in the resulting argv,
// or anywhere `ps auxww` (or a similar process listing) could see it.
const credentialHelperScript = `!f() { test "$1" = get && printf 'password=%s\n' "$MEERKAT_GIT_TOKEN"; }; f` //nolint:gosec // G101: false positive — this is a script *template*; the secret itself is read from $MEERKAT_GIT_TOKEN at runtime, never embedded here.

// resolveGitHubToken returns a cached gh CLI OAuth token, if any. It's a
// package-level var (rather than a direct auth.NewDefault() call) purely
// so tests can stub it without shelling out to a real `gh` binary or
// depending on the test machine's own gh login state (mirrors
// internal/update/release.go's resolveGitHubToken).
var resolveGitHubToken = func() (string, error) {
	return auth.NewDefault().Token(auth.HostGitHub, "")
}

// authClone resolves the URL + extra git invocation (config args, env) to
// use for a clone/fetch of src. Token borrowing is GitHub-only: GitLab (or
// any other host) always gets the plain tokenless URL, so private access
// there depends on the user's own git credential configuration (a full
// clone URL / SSH spec, a credential helper, etc.) rather than anything
// meerkat borrows.
//
// SECURITY: for GitHub, the token — when one is cached via `gh` — is
// handed to git purely through the child process's environment (consumed
// by credentialHelperScript above), never as a command-line argument and
// never written into the clone URL. That keeps it out of both argv
// (visible to any local user via `ps auxww`) and the persisted remote in
// .git/config. When Repo is a full URL/SSH spec, the host isn't github, or
// no cached token is found, this degrades to the plain tokenless URL with
// no extra args — which is exactly what a now-public repo needs for
// anonymous clone/fetch.
func authClone(src Source) (url string, gitArgs, env []string) {
	url = cloneURL(src)
	if strings.Contains(src.Repo, "://") || strings.HasPrefix(src.Repo, "git@") {
		return url, nil, nil // explicit URL/SSH: caller supplies its own auth
	}
	host := src.Host
	if host == "" {
		host = "github"
	}
	if host != "github" {
		return url, nil, nil // no credential borrowing outside GitHub
	}
	tok, err := resolveGitHubToken()
	if err != nil || tok == "" {
		return url, nil, nil // no cached token: proceed anonymously
	}
	url = fmt.Sprintf("https://x-access-token@github.com/%s.git", src.Repo) // username only, no secret
	gitArgs = []string{"-c", "credential.helper=" + credentialHelperScript}
	env = []string{"MEERKAT_GIT_TOKEN=" + tok}
	return url, gitArgs, env
}

// copyTree clears and repopulates the embed dirs from the resolved source per
// layout. Markdown-only for wiki/prompts/templates; the lone sources.yaml is
// copied if present. .gitkeep placeholders are preserved so go:embed always
// has a non-empty directory to bake in.
func copyTree(repoRoot, srcRoot string, l Layout, out io.Writer) error {
	contentDst := filepath.Join(repoRoot, destContent)
	etcDst := filepath.Join(repoRoot, destEtc)

	// wiki -> internal/kb/content
	if err := cleanMarkdown(contentDst); err != nil {
		return err
	}
	n, err := copyMarkdownDir(filepath.Join(srcRoot, l.Wiki), contentDst)
	if err != nil {
		return fmt.Errorf("copy wiki: %w", err)
	}

	// sources.yaml + prompts + templates -> internal/sources/etc
	if err := cleanGlob(etcDst, ".yaml", ".md"); err != nil {
		return err
	}
	if srcFile := filepath.Join(srcRoot, l.Sources); fileExists(srcFile) {
		if err := copyFile(srcFile, filepath.Join(etcDst, "sources.yaml")); err != nil {
			return fmt.Errorf("copy sources.yaml: %w", err)
		}
	}
	np, _ := copyMarkdownDir(filepath.Join(srcRoot, l.Prompts), filepath.Join(etcDst, "prompts"))
	nt, _ := copyMarkdownDir(filepath.Join(srcRoot, l.Templates), filepath.Join(etcDst, "templates"))

	fmt.Fprintf(out, "content: wiki=%d md  prompts=%d  templates=%d\n", n, np, nt)
	return nil
}

// copyMarkdownDir copies *.md from src into dst, preserving subdirectories.
// A missing src dir is not an error (returns 0). Returns the file count.
func copyMarkdownDir(src, dst string) (int, error) {
	if !dirExists(src) {
		return 0, nil
	}
	count := 0
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if err := copyFile(p, filepath.Join(dst, rel)); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // G304: src is a build-time content path from content-source.yaml.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // G304: dst is a fixed package-local embed dir.
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// cleanMarkdown deletes *.md under dir (recursively), keeping everything else
// (notably the .gitkeep placeholder that keeps go:embed valid).
func cleanMarkdown(dir string) error { return cleanGlob(dir, ".md") }

func cleanGlob(dir string, exts ...string) error {
	if !dirExists(dir) {
		return os.MkdirAll(dir, 0o750)
	}
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		for _, e := range exts {
			if strings.HasSuffix(p, e) {
				return os.Remove(p) //nolint:gosec // G122: package-local embed dir under the repo root, not attacker-controlled.
			}
		}
		return nil
	})
}

// localCommit derives a short, stable provenance tag for a local content tree
// by hashing the sorted (relpath, content) of its markdown files.
func localCommit(wikiDir string) string {
	if !dirExists(wikiDir) {
		return "local"
	}
	var lines []string
	_ = filepath.WalkDir(wikiDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil //nolint:nilerr // skip unreadable entries; provenance is best-effort.
		}
		body, rerr := os.ReadFile(p) //nolint:gosec // G304: build-time content path.
		if rerr != nil {
			return nil //nolint:nilerr // best-effort.
		}
		rel, _ := filepath.Rel(wikiDir, p)
		sum := sha256.Sum256(body)
		lines = append(lines, rel+":"+hex.EncodeToString(sum[:8]))
		return nil
	})
	if len(lines) == 0 {
		return "local"
	}
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "local:" + hex.EncodeToString(h[:6])
}

func writeStamp(repoRoot, commit string) (string, error) {
	return commit, os.WriteFile(filepath.Join(repoRoot, StampFile), []byte(commit+"\n"), 0o600)
}

// --- small helpers ---

func runGit(out io.Writer, dir string, args ...string) error {
	return runGitEnv(out, dir, nil, args...)
}

// runGitEnv runs git with extraEnv appended to the current environment —
// used to hand the credential helper its token via env instead of argv.
func runGitEnv(out io.Writer, dir string, extraEnv []string, args ...string) error {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed "git" with config-derived args; no shell.
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, out
	return cmd.Run()
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed "git" with literal args; no shell.
	cmd.Dir = dir
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

func fileExists(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }
func dirExists(p string) bool  { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
func sanitize(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(s)
}

// redact strips an embedded token from a URL for safe logging.
func redact(url string) string {
	if i := strings.Index(url, "@"); i > 0 {
		if j := strings.Index(url, "://"); j >= 0 && j < i {
			return url[:j+3] + "***@" + url[i+1:]
		}
	}
	return url
}
