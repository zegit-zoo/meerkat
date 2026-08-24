package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Version is an opaque token identifying one revision of a stored
// document. It is a content hash for the local backend and an object
// generation for GCS; a caller never interprets it, only echoes it back
// in the next write's precondition.
type Version string

// Precondition is the optimistic-locking condition a write is made
// under. Exactly one of the two states is meaningful:
//
//	Absent            the document must NOT exist (a create)
//	Version != ""     the document must be exactly this revision (an update)
//
// The zero Precondition — neither absent nor versioned — is refused by
// every backend. There is deliberately no "just overwrite whatever is
// there" option: an unconditional write is how two agents saving to the
// same key silently lose one of the two memories, which is the exact
// race this type exists to make impossible.
type Precondition struct {
	Absent  bool
	Version Version
}

// CreateOnly is the precondition for a first write.
func CreateOnly() Precondition { return Precondition{Absent: true} }

// UpdateFrom is the precondition for overwriting a known revision.
func UpdateFrom(v Version) Precondition { return Precondition{Version: v} }

// valid reports whether the precondition names exactly one condition.
func (p Precondition) valid() bool { return p.Absent != (p.Version != "") }

// ErrConflict is returned when a write's precondition did not hold:
// something else created or changed the document first. It is a
// RETRYABLE condition, not a permission failure — the caller re-reads
// and writes again — and the tool surfaces it as such.
var ErrConflict = errors.New("memory changed concurrently")

// ConflictError carries the current revision alongside ErrConflict, so
// a retry does not need a separate read to learn what to condition on.
// Current is empty when the backend could not determine it (the
// document was deleted between the failed write and the re-read).
type ConflictError struct {
	Key     string
	Current Version
}

func (e *ConflictError) Error() string {
	if e.Current == "" {
		return fmt.Sprintf("%v: %s was created or changed by someone else", ErrConflict, e.Key)
	}
	return fmt.Sprintf("%v: %s is now at version %s", ErrConflict, e.Key, e.Current)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// Record is one stored document as Load returns it.
type Record struct {
	// Key is the store-relative name, e.g. "team/deploy-checklist.md".
	Key string
	// Body is the document's bytes.
	Body []byte
	// Version is the revision the bytes were read at.
	Version Version
}

// Backend returns the BOUNDED telemetry label for a store: "local",
// "gcs", or "other" for anything else (a test fake, or a backend added
// later that forgot this function).
//
// It is a type switch rather than a method on Store deliberately. Adding
// a method would widen the interface every implementation — including
// every test fake in this repo — has to satisfy, for a purely
// observational concern. More importantly, the obvious alternative is
// worse: Describe() already returns a human string, and it embeds the
// BUCKET NAME ("gcs://<bucket>/<prefix>"). Using that as a metric label
// or a span attribute would publish a deployment's storage layout on an
// unauthenticated endpoint, which is precisely what the label discipline
// in internal/mcp/metrics.go forbids. This function exists so the
// tempting call is not the available one.
func Backend(s Store) string {
	switch s.(type) {
	case *LocalStore:
		return "local"
	case *GCSStore:
		return "gcs"
	default:
		return "other"
	}
}

// Store is a collection's writable memory backend.
//
// Implementations are safe for concurrent use: the registry shares one
// *Collection (and therefore one Store) across every MCP session.
type Store interface {
	// Load returns every LIVE document, in key order. Anything under
	// StagingPrefix is excluded — a pending artifact must never become
	// readable by being loaded.
	Load(ctx context.Context) ([]Record, error)

	// Stat returns the version key is currently stored at, and whether
	// it exists at all. It is what turns "replace whatever is there" into
	// a compare-and-swap: read the version, then write conditioned on it,
	// so an interleaved write by somebody else still loses the race
	// rather than being silently overwritten.
	Stat(ctx context.Context, key string) (Version, bool, error)

	// Put writes body at key under pre. It returns the new version on
	// success, and a *ConflictError wrapping ErrConflict when the
	// precondition did not hold.
	Put(ctx context.Context, key string, body []byte, pre Precondition) (Version, error)

	// Stage writes a pending review artifact at key (which is always
	// under StagingPrefix) and returns a human-readable location for it.
	// It takes no precondition: a staged artifact is a proposal, and a
	// later proposal for the same key supersedes the earlier one rather
	// than colliding with it.
	Stage(ctx context.Context, key string, body []byte) (string, error)

	// Location renders a store-relative key as something an operator can
	// act on — an absolute path, or a gs:// URL.
	Location(key string) string

	// Describe names the backend for diagnostics, e.g.
	// "local:/srv/kb/memory" or "gcs://bucket/prefix".
	Describe() string
}

// Fingerprinter is implemented by a store that can summarise its LIVE
// document set with a cheap, metadata-only call.
//
// It is the probe half of runtime reconciliation (see internal/refresh):
// two replicas sharing one store converge by comparing fingerprints
// every interval and doing the expensive Load only when the answer
// changed. A store that cannot answer cheaply simply does not implement
// it, and cannot be configured for refresh.
//
// The contract that makes it useful: the fingerprint must change if and
// only if what Load returns would change. It therefore covers exactly
// the objects Load covers — live .md documents, staging excluded — so a
// pending proposal being written does not trigger a fleet-wide reload,
// and a live document being overwritten always does.
//
// It is a separate interface rather than a Store method so that adding a
// backend does not force a fingerprint implementation on it, and so that
// a store which cannot honour the if-and-only-if contract is not
// tempted to approximate one.
type Fingerprinter interface {
	Fingerprint(ctx context.Context) (string, error)
}

// maxDocumentBytes bounds one memory document. A memory is a note, not
// a data dump: every stored document is held in memory, parsed, and
// indexed, and the whole store is loaded at startup. The cap is well
// under internal/kb's own 8 MiB per-page limit, so a document that
// passes here can always be served back.
const maxDocumentBytes = 256 << 10 // 256 KiB

// checkWrite validates the arguments every backend's Put/Stage share.
func checkWrite(key string, body []byte, pre Precondition, needPre bool) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("refusing to store an empty memory document")
	}
	if len(body) > maxDocumentBytes {
		return fmt.Errorf("memory document is %d bytes, over the %d-byte limit", len(body), maxDocumentBytes)
	}
	if needPre && !pre.valid() {
		return errors.New("a memory write needs exactly one precondition: create-only, or a version to update from")
	}
	return nil
}

// Staged reports whether a store key names a pending review artifact.
//
// It checks every path SEGMENT rather than only the leading one, so a
// document sitting under a nested "_staging" directory — which meerkat
// itself never creates, but an operator reorganising the store might —
// is still excluded from Load. Getting this wrong in the permissive
// direction would publish an unreviewed proposal, so it errs the other
// way.
func Staged(key string) bool {
	for _, seg := range strings.Split(key, "/") {
		if seg == StagingPrefix {
			return true
		}
	}
	return false
}

// checkKey re-asserts that a store key is a safe relative path.
//
// Every key reaching a backend was built by Resolve out of a Slug,
// which cannot produce a separator — so this is belt and braces rather
// than the primary defence. It is here because a backend is a public
// type: the day something else constructs a key, the containment
// property must not depend on that caller having read Slug's doc
// comment. The os.Root the local backend writes through is the third
// layer.
func checkKey(key string) error {
	switch {
	case key == "":
		return errors.New("empty memory key")
	case strings.HasPrefix(key, "/"), strings.HasPrefix(key, `\`):
		return fmt.Errorf("memory key %q must be relative", key)
	case strings.Contains(key, `\`):
		return fmt.Errorf("memory key %q must use / as its separator", key)
	case !strings.HasSuffix(key, ".md"):
		return fmt.Errorf("memory key %q must name a .md document", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("memory key %q contains an unsafe path segment", key)
		}
	}
	return nil
}
