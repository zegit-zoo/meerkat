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
