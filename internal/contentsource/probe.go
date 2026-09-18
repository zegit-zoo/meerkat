package contentsource

import (
	"context"
	"fmt"
)

// probe.go is the CHEAP half of runtime reconciliation: "has this source
// changed?", answered with metadata alone.
//
// It exists so the common case — nothing has changed, which is what
// every poll finds nearly all of the time — costs one metadata call and
// nothing else: no download, no extraction, no cache write, no reindex.
// The expensive, hardened path (FetchGCS) runs only once a probe has
// established there is something new to fetch.
//
// The version tokens are exactly the ones FetchGCS keys its cache on, so
// "the probe says X" and "the cache entry named X" are the same claim
// about the same bytes. That is what makes the comparison meaningful:
// see docs/design/hot-reload.md.

// Refreshable reports whether this source is configured for runtime
// reconciliation.
//
// It is the single predicate every caller uses, so the "object stores
// only, never alongside a pinned version" rule enforced at config load
// (see Source.validateRefresh) has exactly one runtime counterpart. A
// pinned source answers false even if a refresh block somehow reached
// here, which keeps the failure direction closed: the worst outcome of
// a validation gap is a source that does not move, never one that does.
func (s Source) Refreshable() bool {
	kind, ok := kindFor(s.Type)
	if !ok || s.Refresh == nil {
		return false
	}
	return kind.pinnedVersion(s) == ""
}

// GCSVersion returns the version token a type: gcs source resolves to
// right now — the object's current generation, or the fingerprint over
// the prefix listing's (name, generation) pairs — using metadata calls
// only.
//
// The token is byte-identical to the one FetchGCS would return for the
// same state, and is compared against the token the collection is
// currently serving. Equal means "nothing to do".
//
// A pinned Generation short-circuits with no call at all: the answer
// cannot change, so asking would be a metadata request whose result is
// already known.
//
// The shared body (storeVersion, objectstore.go) also serves S3Version;
// ObjectVersion dispatches on the source type.
func GCSVersion(ctx context.Context, src Source) (string, error) {
	if src.Type != TypeGCS {
		return "", fmt.Errorf("GCSVersion: type %q is not %q", src.Type, TypeGCS)
	}
	return storeVersion(ctx, src, gcsKind)
}
