package contentsource

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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
// It is the single predicate every caller uses, so the "gcs only, never
// alongside a pinned generation" rule enforced at config load (see
// Source.validateRefresh) has exactly one runtime counterpart. A pinned
// source answers false even if a refresh block somehow reached here,
// which keeps the failure direction closed: the worst outcome of a
// validation gap is a source that does not move, never one that does.
func (s Source) Refreshable() bool {
	return s.Refresh != nil && s.Type == TypeGCS && s.Generation == 0
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
func GCSVersion(ctx context.Context, src Source) (string, error) {
	if src.Type != TypeGCS {
		return "", fmt.Errorf("GCSVersion: type %q is not %q", src.Type, TypeGCS)
	}
	// Re-validated here, as FetchGCS does, so a directly-called probe
	// cannot reach the calls below with a half-specified source.
	if err := src.validateGCS("content"); err != nil {
		return "", err
	}
	if src.Object != "" && src.Generation > 0 {
		return strconv.FormatInt(src.Generation, 10), nil
	}

	client, err := newGCSClient(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()

	if src.Object != "" {
		attrs, aerr := client.Attrs(ctx, src.Bucket, src.Object)
		if aerr != nil {
			return "", fmt.Errorf("gcs://%s/%s: %w", src.Bucket, src.Object, aerr)
		}
		return strconv.FormatInt(attrs.Generation, 10), nil
	}

	objs, lerr := client.Objects(ctx, src.Bucket, src.Prefix)
	if lerr != nil {
		return "", fmt.Errorf("gcs://%s/%s*: %w", src.Bucket, src.Prefix, lerr)
	}
	// quiet: the same skip decisions FetchGCS makes, but without the
	// per-object stderr warning. A probe runs every interval forever, and
	// one permanently oddly-named object in a shared bucket must not
	// produce a warning line per minute for the life of the process. The
	// fetch that follows an actual change still warns, once.
	objs = keepFetchableObjects(objs, src.Prefix, false)
	if len(objs) > maxGCSObjects {
		return "", fmt.Errorf("gcs://%s/%s*: prefix matches %d objects, over the %d-object cap; refusing (narrow the prefix)",
			src.Bucket, src.Prefix, len(objs), maxGCSObjects)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
	return listingFingerprint(objs), nil
}
