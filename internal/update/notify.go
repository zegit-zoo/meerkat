package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// notifyCacheTTL caps how often we hit the GitHub API in the
// background. 24h is plenty — releases are tags, not heartbeats.
const notifyCacheTTL = 24 * time.Hour

// notifyCache is the on-disk shape of the cached check result.
// Lives under $XDG_CACHE_HOME/meerkat/update-check.json (or
// ~/.cache/meerkat/...).
type notifyCache struct {
	CheckedAt time.Time `json:"checked_at"`
	LatestTag string    `json:"latest_tag"`
}

// MaybeNotify emits a one-line "newer version available" message
// to w if the cached check says so. If the cache is stale (>24h),
// fires a fresh check in a goroutine that updates the cache for
// the *next* run — never blocks the current command.
//
// Set MEERKAT_NO_UPDATE_CHECK=1 to disable entirely (useful in
// CI / scripts).
//
// Output format (one line, written to w):
//
//	mk: a newer release is available — v0.4.1 (you have v0.4.0). Run `mk update`.
//
// Errors and missing-cache cases are silent. This is a courtesy,
// not a critical path.
func MaybeNotify(ctx context.Context, currentVersion string, w io.Writer) {
	if os.Getenv("MEERKAT_NO_UPDATE_CHECK") == "1" {
		return
	}
	if w == nil {
		return
	}

	cur := strings.TrimPrefix(currentVersion, "v")
	cur = strings.TrimSuffix(cur, "-dirty")
	if cur == "" || cur == "dev" || cur == "unknown" {
		// Don't nag dev builds — likely the developer themselves.
		return
	}

	cache, err := readNotifyCache()
	switch {
	case err == nil && time.Since(cache.CheckedAt) < notifyCacheTTL:
		// Fresh enough — emit the nag if needed; no network call.
		emitNagIfNewer(w, currentVersion, cache.LatestTag)
	default:
		// Cache missing / stale — fire a refresh in the background
		// so the *next* command run gets a current answer. Don't
		// block this run.
		//
		// #nosec G118 -- context.Background is intentional: this
		// goroutine MUST outlive the parent CLI command. Binding to
		// the request-scoped ctx would cancel the refresh the
		// moment the user's command exits, which is precisely when
		// the refresh becomes useful (it populates the cache for
		// the *next* invocation).
		go refreshNotifyCache(context.Background())
		// If we have an old (stale) cache, still emit a stale-data
		// nag — better to remind the user than to be silent for the
		// 200ms it takes to refresh.
		if err == nil {
			emitNagIfNewer(w, currentVersion, cache.LatestTag)
		}
	}
}

func emitNagIfNewer(w io.Writer, currentVersion, latestTag string) {
	if latestTag == "" {
		return
	}
	if !isNewer(latestTag, currentVersion) {
		return
	}
	fmt.Fprintf(w,
		"\nmk: a newer release is available — %s (you have %s). Run `mk update`.\n",
		latestTag, currentVersion,
	)
}

// isNewer is a deliberately conservative semver-ish compare. We
// don't pull a full semver lib for one comparison; the tag format
// we ship is `vX.Y.Z` and we just dot-split + numeric-compare.
// Any parse failure means "not newer" so we never spam.
func isNewer(latest, current string) bool {
	l, ok := splitVersion(latest)
	if !ok {
		return false
	}
	c, ok := splitVersion(current)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// splitVersion parses "vX.Y.Z" (with optional pre-release suffix
// like "-dirty" or "-rc1") into [3]int. Returns ok=false on shapes
// it can't make sense of.
func splitVersion(v string) ([3]int, bool) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 4)
	if len(parts) < 3 {
		return [3]int{}, false
	}
	out := [3]int{}
	for i := 0; i < 3; i++ {
		n := 0
		any := false
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
			any = true
		}
		if !any {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// refreshNotifyCache hits FetchLatest and writes the cache. Best-
// effort — silently fails on auth / network problems.
func refreshNotifyCache(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rel, err := FetchLatest(c)
	if err != nil {
		// Still touch the cache so we don't try again on every CLI
		// invocation if e.g. gh isn't auth'd (or the network is down).
		// Mark with empty LatestTag so emitNagIfNewer is a no-op.
		_ = writeNotifyCache(notifyCache{CheckedAt: time.Now(), LatestTag: ""})
		return
	}
	_ = writeNotifyCache(notifyCache{CheckedAt: time.Now(), LatestTag: rel.TagName})
}

func notifyCachePath() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "meerkat", "update-check.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "meerkat", "update-check.json")
}

func readNotifyCache() (notifyCache, error) {
	var c notifyCache
	p := notifyCachePath()
	if p == "" {
		return c, fmt.Errorf("no cache path")
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return c, err
	}
	return c, nil
}

func writeNotifyCache(c notifyCache) error {
	p := notifyCachePath()
	if p == "" {
		return fmt.Errorf("no cache path")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
