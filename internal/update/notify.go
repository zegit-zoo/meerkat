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
	if !IsUpgrade(latestTag, currentVersion) {
		return
	}
	fmt.Fprintf(w,
		"\nmk: a newer release is available — %s (you have %s). Run `mk update`.\n",
		latestTag, currentVersion,
	)
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
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// Write to an unpredictable temp name (os.CreateTemp, which also
	// gives O_EXCL) rather than the fixed "<p>.tmp", then rename into
	// place. Same bug class as copyFile in install.go: a fixed name
	// under a directory this process can write to can be pre-planted
	// as a symlink to an arbitrary file, which a plain os.WriteFile
	// (open + O_TRUNC, following symlinks) would then clobber.
	tmp, err := os.CreateTemp(dir, ".update-check-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, p); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
