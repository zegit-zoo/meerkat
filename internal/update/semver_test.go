package update

import "testing"

// TestIsUpgrade covers the version compare matrix used by both the
// "newer release available" nag (notify.go) and mk update's downgrade
// guard. Always-false on parse failure keeps us from ever claiming an
// upgrade we can't actually confirm.
func TestIsUpgrade(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v0.4.1", "v0.4.0", true},
		{"v0.5.0", "v0.4.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.4.0", "v0.4.0", false},
		{"v0.4.0", "v0.4.0-dirty", false}, // same version, dirty ignored
		{"v0.4.0", "v0.5.0", false},       // older candidate
		{"", "v0.4.0", false},             // missing candidate
		{"v0.4.1", "", false},             // missing current
		{"garbage", "v0.4.0", false},      // unparseable candidate
		{"v0.4.0", "garbage", false},      // unparseable current
		{"v0.4.1", "v0.4.0-dirty", true},  // newer real release vs dirty current

		// Real SemVer pre-release precedence (spec item 11): unlike the
		// old dot-split compare this replaces, a genuine pre-release
		// suffix is now compared, not silently ignored just because the
		// X.Y.Z core matches.
		{"v0.4.0-rc1", "v0.4.0-rc0", true},  // rc1 > rc0 lexically
		{"v0.4.0-rc0", "v0.4.0-rc1", false}, // rc0 < rc1
		{"v0.4.0", "v0.4.0-rc1", true},      // a real release beats any pre-release of it
		{"v0.4.0-rc1", "v0.4.0", false},     // a pre-release never beats the release it precedes

		// The scenario issue #13 is actually about: a downstream fork
		// deployment sitting on an advanced patch series (v0.8.6) must
		// still recognize a much-lower-numbered-repo's new upstream
		// v0.9.0+ tag as an upgrade — SemVer precedence doesn't care
		// which repository originally minted a tag, only the numbers.
		{"v0.9.0", "v0.8.6", true},
		{"v1.0.0", "v0.8.6", true},
		{"v0.8.6", "v0.8.6", false}, // exact match is not an upgrade
		{"v0.8.5", "v0.8.6", false}, // an older upstream tag is not an upgrade
		{"v0.8.10", "v0.8.6", true}, // numeric patch compare, not string compare (10 > 6, "10" < "6" lexically)
		{"v0.10.0", "v0.9.9", true}, // numeric minor compare, not string compare
	}
	for _, tc := range cases {
		if got := IsUpgrade(tc.candidate, tc.current); got != tc.want {
			t.Errorf("IsUpgrade(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

// TestIsDowngrade covers mk update's downgrade guard directly. It must
// agree with IsUpgrade's notion of ordering (target < current), and
// must never block on an unparseable version on either side.
func TestIsDowngrade(t *testing.T) {
	cases := []struct {
		target, current string
		want            bool
	}{
		{"v0.8.5", "v0.8.6", true},   // genuine downgrade
		{"v0.8.6", "v0.8.6", false},  // exact reinstall is not a "downgrade"
		{"v0.9.0", "v0.8.6", false},  // upstream migration case: not a downgrade
		{"v1.0.0", "v0.8.6", false},  // ditto, larger jump
		{"", "v0.8.6", false},        // unparseable target never blocks
		{"v0.8.5", "dev", false},     // unparseable current (dev build) never blocks
		{"v0.8.5", "unknown", false}, // unparseable current never blocks
	}
	for _, tc := range cases {
		if got := IsDowngrade(tc.target, tc.current); got != tc.want {
			t.Errorf("IsDowngrade(%q, %q) = %v, want %v", tc.target, tc.current, got, tc.want)
		}
	}
}

// TestParseSemver confirms ok=false on shapes we can't parse, and that
// the "v" prefix, "-dirty" dev-build marker, and build metadata are
// all handled per spec.
func TestParseSemver(t *testing.T) {
	okCases := []string{"v0.4.0", "0.4.0", "v0.4.0-dirty", "v0.4.0-rc.1", "v1.2.3+build.5"}
	for _, v := range okCases {
		if _, ok := parseSemver(v); !ok {
			t.Errorf("parseSemver(%q) should parse", v)
		}
	}
	badCases := []string{"v0.4", "dev", "unknown", "", "v0..0", "v0.4.x"}
	for _, v := range badCases {
		if _, ok := parseSemver(v); ok {
			t.Errorf("parseSemver(%q) should NOT parse", v)
		}
	}
}

// TestParseSemver_BuildMetadataIgnoredForPrecedence: two versions
// differing only in build metadata compare equal (spec item 10).
func TestParseSemver_BuildMetadataIgnoredForPrecedence(t *testing.T) {
	a, ok := parseSemver("v1.2.3+build.1")
	if !ok {
		t.Fatal("v1.2.3+build.1 should parse")
	}
	b, ok := parseSemver("v1.2.3+build.999")
	if !ok {
		t.Fatal("v1.2.3+build.999 should parse")
	}
	if compareSemver(a, b) != 0 {
		t.Errorf("versions differing only in build metadata should compare equal")
	}
}
