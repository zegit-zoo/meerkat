package update

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIsNewer covers the version compare matrix. Always-false on
// parse failure keeps us from ever spamming.
func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.4.1", "v0.4.0", true},
		{"v0.5.0", "v0.4.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.4.0", "v0.4.0", false},
		{"v0.4.0", "v0.4.0-dirty", false},   // same version, dirty ignored
		{"v0.4.0", "v0.5.0", false},         // older latest
		{"", "v0.4.0", false},               // missing latest
		{"v0.4.1", "", false},               // missing current
		{"garbage", "v0.4.0", false},        // unparseable latest
		{"v0.4.0", "garbage", false},        // unparseable current
		{"v0.4.0-rc1", "v0.4.0-rc0", false}, // pre-release suffix ignored — same x.y.z
		{"v0.4.1", "v0.4.0-dirty", true},    // newer real release vs dirty current
	}
	for _, tc := range cases {
		if got := isNewer(tc.latest, tc.current); got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

// TestSplitVersion confirms ok=false on shapes we can't parse.
func TestSplitVersion(t *testing.T) {
	if _, ok := splitVersion("v0.4.0"); !ok {
		t.Error("v0.4.0 should parse")
	}
	if _, ok := splitVersion("0.4.0"); !ok {
		t.Error("0.4.0 should parse")
	}
	if _, ok := splitVersion("v0.4.0-dirty"); !ok {
		t.Error("v0.4.0-dirty should parse (suffix ignored)")
	}
	if _, ok := splitVersion("v0.4"); ok {
		t.Error("v0.4 should NOT parse (only 2 segments)")
	}
	if _, ok := splitVersion("dev"); ok {
		t.Error("'dev' should NOT parse")
	}
	if _, ok := splitVersion(""); ok {
		t.Error("empty should NOT parse")
	}
}

// TestMaybeNotify_DevBuildSilent: dev / unknown current versions
// suppress the nag entirely.
func TestMaybeNotify_DevBuildSilent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	// Pre-seed cache with a newer release.
	mustWriteCache(t, tmp, notifyCache{
		CheckedAt: time.Now(),
		LatestTag: "v9.9.9",
	})
	for _, ver := range []string{"dev", "unknown", ""} {
		var buf bytes.Buffer
		MaybeNotify(context.Background(), ver, &buf)
		if buf.Len() != 0 {
			t.Errorf("expected silent for current=%q, got %q", ver, buf.String())
		}
	}
}

// TestMaybeNotify_EmitsWhenCachedNewer.
func TestMaybeNotify_EmitsWhenCachedNewer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	mustWriteCache(t, tmp, notifyCache{
		CheckedAt: time.Now(),
		LatestTag: "v0.5.0",
	})
	var buf bytes.Buffer
	MaybeNotify(context.Background(), "v0.4.0", &buf)
	out := buf.String()
	if !strings.Contains(out, "v0.5.0") || !strings.Contains(out, "v0.4.0") {
		t.Errorf("nag missing version info: %q", out)
	}
	if !strings.Contains(out, "mk update") {
		t.Errorf("nag missing 'mk update' hint: %q", out)
	}
}

// TestMaybeNotify_QuietWhenSameVersion.
func TestMaybeNotify_QuietWhenSameVersion(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	mustWriteCache(t, tmp, notifyCache{
		CheckedAt: time.Now(),
		LatestTag: "v0.4.0",
	})
	var buf bytes.Buffer
	MaybeNotify(context.Background(), "v0.4.0", &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent at same version, got %q", buf.String())
	}
}

// TestMaybeNotify_NoUpdateCheckEnv kills the whole feature.
func TestMaybeNotify_NoUpdateCheckEnv(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("MEERKAT_NO_UPDATE_CHECK", "1")
	mustWriteCache(t, tmp, notifyCache{
		CheckedAt: time.Now(),
		LatestTag: "v9.9.9",
	})
	var buf bytes.Buffer
	MaybeNotify(context.Background(), "v0.4.0", &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent under MEERKAT_NO_UPDATE_CHECK=1, got %q", buf.String())
	}
}

// TestMaybeNotify_StaleCacheStillEmits — when cache is older than
// the TTL, we still emit if we have a value (better to remind than
// be silent during the background refresh).
func TestMaybeNotify_StaleCacheStillEmits(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	mustWriteCache(t, tmp, notifyCache{
		CheckedAt: time.Now().Add(-48 * time.Hour),
		LatestTag: "v0.5.0",
	})
	var buf bytes.Buffer
	MaybeNotify(context.Background(), "v0.4.0", &buf)
	if !strings.Contains(buf.String(), "v0.5.0") {
		t.Errorf("expected nag from stale cache, got %q", buf.String())
	}
}

// TestNotifyCacheRoundTrip: write + read works.
func TestNotifyCacheRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	want := notifyCache{
		CheckedAt: time.Now().Truncate(time.Second),
		LatestTag: "v0.4.2",
	}
	if err := writeNotifyCache(want); err != nil {
		t.Fatal(err)
	}
	got, err := readNotifyCache()
	if err != nil {
		t.Fatal(err)
	}
	if !got.CheckedAt.Equal(want.CheckedAt) || got.LatestTag != want.LatestTag {
		t.Errorf("roundtrip diverged: want %+v got %+v", want, got)
	}
}

// TestWriteNotifyCache_DoesNotFollowPreplantedSymlinkAtFixedTmpName is
// the regression test for the notify-cache half of the symlink-follow
// finding: writeNotifyCache used to write its body via
// os.WriteFile(p+".tmp", ...), a fixed, guessable name. A symlink
// planted at that exact path ahead of time — same trick as the
// install.go finding — would get written through and left with the
// cache's permissions. Confirm the pre-planted symlink at that legacy
// path is left untouched and the real cache write still succeeds via
// an unguessable temp name.
func TestWriteNotifyCache_DoesNotFollowPreplantedSymlinkAtFixedTmpName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)

	cacheDir := filepath.Join(tmp, "meerkat")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}

	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("victim-content"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	// Pre-plant a symlink at the OLD fixed name ("<cache path>.tmp").
	legacyTmpPath := filepath.Join(cacheDir, "update-check.json.tmp")
	if err := os.Symlink(victim, legacyTmpPath); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := writeNotifyCache(notifyCache{CheckedAt: time.Now(), LatestTag: "v9.9.9"}); err != nil {
		t.Fatalf("writeNotifyCache: %v", err)
	}

	// Victim untouched.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != "victim-content" {
		t.Fatalf("victim clobbered via pre-planted symlink: %q", got)
	}

	// The pre-planted symlink itself must be untouched too.
	fi, err := os.Lstat(legacyTmpPath)
	if err != nil {
		t.Fatalf("lstat pre-planted symlink: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pre-planted symlink at %q was replaced with a regular file", legacyTmpPath)
	}

	// The real cache write must have gone through via a different,
	// unguessable temp name and be readable back.
	c, err := readNotifyCache()
	if err != nil {
		t.Fatalf("readNotifyCache: %v", err)
	}
	if c.LatestTag != "v9.9.9" {
		t.Fatalf("cache LatestTag = %q, want v9.9.9", c.LatestTag)
	}
}

func mustWriteCache(t *testing.T, root string, c notifyCache) {
	t.Helper()
	dir := filepath.Join(root, "meerkat")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, "update-check.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}
