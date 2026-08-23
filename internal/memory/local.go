package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// LocalStore is a memory store backed by a directory on disk.
//
// # Durability
//
// Every write is temp-file + atomic rename within the same directory,
// so a reader (this process on the next restart, an operator, a backup)
// never observes a half-written document, and a crash mid-write leaves
// either the old document or the new one.
//
// # Optimistic locking
//
// A version is the sha256 of the stored bytes. Put takes the store's
// write lock, checks the precondition against what is on disk right
// now, and only then renames — so two goroutines racing to create the
// same key produce exactly one success and one ErrConflict, and two
// racing to update from the same version likewise.
//
// The lock is in-process. Two meerkat processes writing the SAME local
// directory are outside what this backend can serialise (POSIX gives no
// portable compare-and-swap on a file), and that is documented rather
// than papered over: a deployment that needs multi-writer memory should
// use the GCS backend, whose preconditions are enforced by the
// backend itself. See docs/design/memory.md.
type LocalStore struct {
	root *os.Root
	dir  string

	// mu serialises the read-check-write sequence in Put. One lock for
	// the whole store rather than one per key: memory writes are rare
	// (a human-or-agent-scale event, not a request-rate one), and a
	// single lock is a property you can check by reading one function
	// instead of an invariant about a map of locks.
	mu sync.Mutex
}

// OpenLocal opens (creating it if needed) a local memory store at dir.
func OpenLocal(dir string) (*LocalStore, error) {
	if dir == "" {
		return nil, errors.New("local memory store needs a directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve memory directory %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create memory directory %q: %w", abs, err)
	}
	// SECURITY: os.Root, for the same reason internal/kbdir uses one —
	// it resolves each path component and refuses to traverse out of the
	// tree, including through a symlink stored inside it. A memory store
	// is a directory meerkat WRITES into on behalf of a remote caller,
	// so containment cannot rest on the key having been built correctly.
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open memory directory %q: %w", abs, err)
	}
	return &LocalStore{root: root, dir: abs}, nil
}

// Close releases the store's directory handle.
func (s *LocalStore) Close() error {
	if s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	return err
}

// Describe implements Store.
func (s *LocalStore) Describe() string { return "local:" + s.dir }

// Location implements Store.
func (s *LocalStore) Location(key string) string {
	return filepath.Join(s.dir, filepath.FromSlash(key))
}

// Load implements Store: every live .md document under the root, in key
// order, with StagingPrefix skipped.
func (s *LocalStore) Load(_ context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Record
	err := fs.WalkDir(s.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if Staged(p) {
				return fs.SkipDir
			}
			return nil
		}
		// Skip anything that isn't a regular file: with an
		// operator-writable directory the tree can hold FIFOs and device
		// nodes, and opening a FIFO blocks forever. Same reasoning as
		// kb.ListFS.
		if !d.Type().IsRegular() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		body, rerr := s.readFile(p)
		if rerr != nil {
			// One unreadable document must not make a whole collection
			// unserveable — it would take the process down at startup.
			fmt.Fprintf(os.Stderr, "meerkat: skipping memory %s: %v\n", p, rerr)
			return nil
		}
		out = append(out, Record{Key: p, Body: body, Version: hashVersion(body)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read memory store %s: %w", s.dir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Stat implements Store.
func (s *LocalStore) Stat(_ context.Context, key string) (Version, bool, error) {
	if err := checkKey(key); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentVersion(key)
}

// Put implements Store.
func (s *LocalStore) Put(_ context.Context, key string, body []byte, pre Precondition) (Version, error) {
	if err := checkWrite(key, body, pre, true); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current, exists, err := s.currentVersion(key)
	if err != nil {
		return "", err
	}
	switch {
	case pre.Absent && exists:
		return "", &ConflictError{Key: key, Current: current}
	case !pre.Absent && !exists:
		// The document the caller meant to update is gone. That is a
		// conflict, not a create: silently creating it would resurrect a
		// memory somebody deliberately removed.
		return "", &ConflictError{Key: key}
	case !pre.Absent && current != pre.Version:
		return "", &ConflictError{Key: key, Current: current}
	}
	if err := s.write(key, body); err != nil {
		return "", err
	}
	return hashVersion(body), nil
}

// Stage implements Store: an unconditional write into the staging
// prefix.
func (s *LocalStore) Stage(_ context.Context, key string, body []byte) (string, error) {
	if err := checkWrite(key, body, Precondition{}, false); err != nil {
		return "", err
	}
	if !strings.HasPrefix(key, StagingPrefix+"/") {
		return "", fmt.Errorf("staged memory key %q must be under %s/", key, StagingPrefix)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(key, body); err != nil {
		return "", err
	}
	return s.Location(key), nil
}

// currentVersion reports the version of key on disk right now.
func (s *LocalStore) currentVersion(key string) (Version, bool, error) {
	body, err := s.readFile(key)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read memory %q: %w", key, err)
	}
	return hashVersion(body), true, nil
}

// readFile reads one document through the store's os.Root, capped.
func (s *LocalStore) readFile(key string) ([]byte, error) {
	f, err := s.root.Open(filepath.FromSlash(key))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDocumentBytes {
		return nil, fmt.Errorf("memory %q exceeds the %d-byte limit", key, maxDocumentBytes)
	}
	return body, nil
}

// write does the temp-file + atomic-rename dance, entirely inside the
// store's os.Root. The caller holds s.mu.
func (s *LocalStore) write(key string, body []byte) error {
	if dir := path.Dir(key); dir != "." {
		if err := s.root.MkdirAll(filepath.FromSlash(dir), 0o750); err != nil {
			return fmt.Errorf("create memory directory for %q: %w", key, err)
		}
	}
	// The temp file is a SIBLING of the target: os.Rename is only atomic
	// within one filesystem, and the store root may well be a mount of
	// its own.
	tmp := tempName(key)
	f, err := s.root.OpenFile(filepath.FromSlash(tmp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file for %q: %w", key, err)
	}
	cleanup := func() { _ = s.root.Remove(filepath.FromSlash(tmp)) }
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("write memory %q: %w", key, err)
	}
	// Sync before rename: a rename is atomic with respect to other
	// readers, but says nothing about the data having reached the disk.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("sync memory %q: %w", key, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close memory %q: %w", key, err)
	}
	if err := s.root.Rename(filepath.FromSlash(tmp), filepath.FromSlash(key)); err != nil {
		cleanup()
		return fmt.Errorf("finalize memory %q: %w", key, err)
	}
	return nil
}

// tempName is the in-place staging name for a write to key. It carries
// a dot prefix so a crash leaves something obviously transient, and the
// process ID so two processes sharing a directory do not fight over one
// temp name even though they cannot serialise the write itself.
func tempName(key string) string {
	dir, base := path.Split(key)
	return path.Join(dir, fmt.Sprintf(".%s.%d.tmp", base, os.Getpid()))
}

// hashVersion is the local backend's version token: a truncated sha256
// of the stored bytes.
//
// Content-addressing rather than mtime/size means the token cannot
// collide across a fast rewrite (two writes inside one filesystem
// timestamp tick), and means a write that stores byte-identical content
// leaves the version unchanged — which is correct: nothing changed, so
// nobody's precondition should be invalidated.
func hashVersion(body []byte) Version {
	sum := sha256.Sum256(body)
	return Version(hex.EncodeToString(sum[:])[:16])
}
