// Package gitinfo reads a git working tree's identity from git's own
// files, and runs the few git commands meerkat-mob#25 allows, hardened.
//
// # Reading, never running (MK-SEC-12)
//
// A freshness probe must not invoke git: pointing meerkat at a knowledge
// base must never become code execution, and a git invocation honours
// that repository's hooks and configuration. So everything in this file
// reads plain files — `.git`, `HEAD`, loose refs, `packed-refs`, the
// `config` file and pack indexes — with a size cap on every read, and
// executes nothing. The opt-in commands (remote.go) are the only exec in
// the package.
//
// What it deliberately does NOT follow:
//
//   - `[include]` / `[includeIf]` in config. An upstream defined only in
//     an included file is not seen, and the remote reads as unknown.
//   - `url.<base>.insteadOf` rewrites. The remote URL is used exactly as
//     the repository's config spells it (the review of #25 part B asked
//     for this: a rewrite in the KB's own config must not redirect the
//     remote check).
//   - Object store alternates. An object that may live in an alternate
//     is reported as not knowable (ErrCapped) rather than as absent.
package gitinfo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrNotRepo means no git working tree contains the directory.
	ErrNotRepo = errors.New("not inside a git working tree")
	// ErrDetached means HEAD names a commit, not a branch.
	ErrDetached = errors.New("HEAD is detached")
	// ErrNoUpstream means the current branch tracks no remote branch.
	ErrNoUpstream = errors.New("the branch tracks no remote branch")
	// ErrCapped means a bounded lookup gave up before it could answer.
	ErrCapped = errors.New("lookup capped before it could answer")
	// ErrUnsafeName is a remote or branch name from the repository's
	// config that could be read as a command-line option, or is not a
	// plain name. It is refused before any git call sees it.
	ErrUnsafeName = errors.New("refused a remote or branch name")
)

// Size caps for the files this package reads. None of them is large in a
// healthy repository; a cap turns a pathological one into an error
// rather than a probe that stalls or balloons.
const (
	maxSmallFile  = 4 << 10  // .git file, HEAD, a loose ref, commondir
	maxConfigFile = 1 << 20  // config
	maxPackedRefs = 64 << 20 // packed-refs
	maxWalkUp     = 64       // directory levels searched for .git
)

// Repo is one git working tree, located but not trusted: every path in it
// came from files the repository controls, and is only ever read.
type Repo struct {
	// Root is the working tree root, the directory holding `.git`.
	Root string
	// gitDir holds HEAD; commonDir holds refs, packed-refs, config and
	// objects. They differ for a linked worktree.
	gitDir    string
	commonDir string
}

// Find returns the working tree containing dir, searching dir and its
// parents, as git does. It never looks further than maxWalkUp levels.
func Find(dir string) (*Repo, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for range maxWalkUp {
		candidate := filepath.Join(d, ".git")
		fi, err := os.Stat(candidate)
		switch {
		case err == nil && fi.IsDir():
			return newRepo(d, candidate)
		case err == nil && fi.Mode().IsRegular():
			gitDir, err := readGitdirFile(candidate)
			if err != nil {
				return nil, err
			}
			return newRepo(d, gitDir)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return nil, ErrNotRepo
}

// readGitdirFile resolves a `.git` FILE (a linked worktree or a
// submodule): one line, `gitdir: <path>`, relative to the file's
// directory when not absolute.
func readGitdirFile(path string) (string, error) {
	b, err := readCapped(path, maxSmallFile)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(b))
	target, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s: not a gitdir file", path)
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return filepath.Clean(target), nil
}

func newRepo(root, gitDir string) (*Repo, error) {
	r := &Repo{Root: root, gitDir: gitDir, commonDir: gitDir}
	if b, err := readCapped(filepath.Join(gitDir, "commondir"), maxSmallFile); err == nil {
		common := strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		r.commonDir = filepath.Clean(common)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return r, nil
}

// Head returns the commit HEAD resolves to and the branch it is on. A
// detached HEAD returns the commit and ErrDetached.
func (r *Repo) Head() (commit, branch string, err error) {
	b, err := readCapped(filepath.Join(r.gitDir, "HEAD"), maxSmallFile)
	if err != nil {
		return "", "", err
	}
	content := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(content, "ref:"); ok {
		ref = strings.TrimSpace(ref)
		commit, err := r.resolveRef(ref, 0)
		if err != nil {
			return "", "", err
		}
		return commit, strings.TrimPrefix(ref, "refs/heads/"), nil
	}
	if !isObjectName(content) {
		return "", "", fmt.Errorf("HEAD: neither a ref nor an object name")
	}
	return content, "", ErrDetached
}

// resolveRef follows a ref to the object it names: a loose ref file (the
// worktree's own first, then the shared one), else packed-refs.
func (r *Repo) resolveRef(ref string, depth int) (string, error) {
	if depth > 5 {
		return "", fmt.Errorf("ref %q: too many symbolic levels", ref)
	}
	if err := checkRefName(ref); err != nil {
		return "", err
	}
	for _, base := range []string{r.gitDir, r.commonDir} {
		b, err := readCapped(filepath.Join(base, filepath.FromSlash(ref)), maxSmallFile)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		content := strings.TrimSpace(string(b))
		if next, ok := strings.CutPrefix(content, "ref:"); ok {
			return r.resolveRef(strings.TrimSpace(next), depth+1)
		}
		if !isObjectName(content) {
			return "", fmt.Errorf("ref %q: not an object name", ref)
		}
		return content, nil
	}
	return r.packedRef(ref)
}

// packedRef looks ref up in packed-refs: `<name> <ref>` lines, with `#`
// headers and `^` peeled lines skipped.
func (r *Repo) packedRef(ref string) (string, error) {
	path := filepath.Join(r.commonDir, "packed-refs")
	if err := mustBeRegular(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	f, err := os.Open(path) //nolint:gosec // G304: packed-refs of the working tree meerkat was pointed at; read-only, size-capped.
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("ref %q: not found", ref)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxPackedRefs))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		name, rest, ok := strings.Cut(line, " ")
		if ok && rest == ref && isObjectName(name) {
			return name, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("ref %q: not found", ref)
}

// Upstream is what a branch tracks.
type Upstream struct {
	// Remote is the remote's name in the repository's config.
	Remote string
	// URL is remote.<name>.url, exactly as configured: no insteadOf
	// rewrite is applied.
	URL string
	// Branch is the remote branch, without refs/heads/.
	Branch string
}

// Upstream returns what branch tracks, read from the config file.
func (r *Repo) Upstream(branch string) (Upstream, error) {
	cfg, err := r.config()
	if err != nil {
		return Upstream{}, err
	}
	remote := cfg.get("branch", branch, "remote")
	merge := cfg.get("branch", branch, "merge")
	if remote == "" || merge == "" || remote == "." {
		return Upstream{}, ErrNoUpstream
	}
	remoteBranch := strings.TrimPrefix(merge, "refs/heads/")
	// Both names reach `git pull` as arguments, and git forwards them to
	// fetch WITHOUT a `--`: a name starting with `-` would be an option
	// there (`--upload-pack=…` runs a command). Refused here, and again
	// in PullFFOnly.
	if err := CheckName(remote); err != nil {
		return Upstream{}, fmt.Errorf("remote: %w", err)
	}
	if err := CheckName(remoteBranch); err != nil {
		return Upstream{}, fmt.Errorf("branch: %w", err)
	}
	url := cfg.first("remote", remote, "url")
	if url == "" {
		return Upstream{}, fmt.Errorf("remote %q has no url", remote)
	}
	return Upstream{Remote: remote, URL: url, Branch: remoteBranch}, nil
}

// CheckName refuses a remote or branch name that could be read as an
// option, or that is not a plain name: empty, starting with `-`,
// containing whitespace, a control character, `..`, `:` or a backslash.
// It is stricter than git, and errs toward refusing.
func CheckName(name string) error {
	if name == "" || name[0] == '-' || strings.Contains(name, "..") || strings.ContainsAny(name, ":\\") {
		return fmt.Errorf("%w: %q", ErrUnsafeName, name)
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w: %q", ErrUnsafeName, name)
		}
	}
	return nil
}

// Limits bound HasObject.
type Limits struct {
	// MaxPacks is how many pack indexes may be searched.
	MaxPacks int
	// Budget is the wall-clock time the whole lookup may take.
	Budget time.Duration
}

// DefaultLimits keeps a probe of a very large repository from stalling:
// past either bound the answer is ErrCapped, which a caller reports as
// unknown.
var DefaultLimits = Limits{MaxPacks: 64, Budget: 250 * time.Millisecond}

// HasObject reports whether the repository's object store holds name,
// as a loose object or in a pack index. It reads fixed-size records from
// each index (a fanout lookup, then a binary search), never whole files.
func (r *Repo) HasObject(ctx context.Context, name string, lim Limits) (bool, error) {
	if !isObjectName(name) {
		return false, fmt.Errorf("%q is not an object name", name)
	}
	raw, err := hex.DecodeString(name)
	if err != nil {
		return false, err
	}
	objects := filepath.Join(r.commonDir, "objects")
	if _, err := os.Stat(filepath.Join(objects, name[:2], name[2:])); err == nil {
		return true, nil
	}
	entries, err := os.ReadDir(filepath.Join(objects, "pack"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var idx []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".idx") {
			idx = append(idx, filepath.Join(objects, "pack", e.Name()))
		}
	}
	if len(idx) > lim.MaxPacks {
		return false, fmt.Errorf("%w: %d pack indexes, over %d", ErrCapped, len(idx), lim.MaxPacks)
	}
	deadline := time.Now().Add(lim.Budget)
	for _, p := range idx {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("%w: over the %s budget", ErrCapped, lim.Budget)
		}
		found, err := idxContains(p, raw)
		if err != nil {
			return false, fmt.Errorf("%w: %s: %v", ErrCapped, filepath.Base(p), err)
		}
		if found {
			return true, nil
		}
	}
	if _, err := os.Stat(filepath.Join(objects, "info", "alternates")); err == nil {
		return false, fmt.Errorf("%w: the object store has alternates, which are not searched", ErrCapped)
	}
	return false, nil
}

// idxContains searches one version-2 pack index for name.
func idxContains(path string, name []byte) (bool, error) {
	if err := mustBeRegular(path); err != nil {
		return false, err
	}
	f, err := os.Open(path) //nolint:gosec // G304: a pack index inside the working tree meerkat was pointed at; read with fixed-size ReadAt calls, never executed.
	if err != nil {
		return false, err
	}
	defer f.Close()
	var hdr [8 + 256*4]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return false, fmt.Errorf("read header: %w", err)
	}
	if !bytes.Equal(hdr[:4], []byte{0xff, 't', 'O', 'c'}) || binary.BigEndian.Uint32(hdr[4:8]) != 2 {
		return false, errors.New("not a version-2 pack index")
	}
	fanout := func(i int) int64 { return int64(binary.BigEndian.Uint32(hdr[8+4*i:])) }
	b := int(name[0])
	lo, hi := int64(0), fanout(b)
	if b > 0 {
		lo = fanout(b - 1)
	}
	width := int64(len(name))
	buf := make([]byte, width)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := f.ReadAt(buf, int64(len(hdr))+mid*width); err != nil {
			return false, fmt.Errorf("read entry: %w", err)
		}
		switch c := bytes.Compare(buf, name); {
		case c == 0:
			return true, nil
		case c < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return false, nil
}

// isObjectName accepts a SHA-1 (40) or SHA-256 (64) hex object name.
func isObjectName(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// checkRefName refuses a ref that could escape the git directory when
// joined to it. It is stricter than git: a name git would accept but
// this refuses reads as an error, never as a path outside the repo.
func checkRefName(ref string) error {
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\\\x00") {
		return fmt.Errorf("ref %q: refused", ref)
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("ref %q: refused", ref)
		}
	}
	return nil
}

// mustBeRegular refuses anything but a regular file before it is opened:
// a FIFO planted at `.git/HEAD` or as a pack index would otherwise block
// the probe on open, as kb's page walk already guards against for pages.
func mustBeRegular(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}
	return nil
}

// readCapped reads at most max bytes of a file, refusing a larger one.
func readCapped(path string, max int64) ([]byte, error) {
	if err := mustBeRegular(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // G304: git's own files in the working tree meerkat was pointed at; read-only, size-capped, parsed and never executed.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: over %d bytes", path, max)
	}
	return b, nil
}
