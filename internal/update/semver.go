package update

import (
	"strconv"
	"strings"
)

// semver is a parsed SemVer 2.0.0 (https://semver.org) version: the
// three required numeric components plus an optional dot-separated
// pre-release identifier list. Build metadata (a "+..." suffix) is
// intentionally not represented — the spec (item 10) says it MUST be
// ignored when determining precedence, so parseSemver discards it
// before this struct is ever populated.
type semver struct {
	major, minor, patch int
	// prerelease holds the dot-separated identifiers after a "-", or
	// nil for a normal (non-pre-release) version. A nil/empty slice
	// and "no pre-release" are the same thing here.
	prerelease []string
}

// parseSemver parses v into a semver, tolerating:
//   - an optional leading "v" (this project's tags are "vX.Y.Z")
//   - a trailing "-dirty" marker (this project's dev-build convention,
//     stripped before pre-release parsing — it's a local build marker,
//     not a real SemVer pre-release identifier)
//   - build metadata ("+...", ignored per spec item 10)
//   - a real SemVer pre-release suffix ("-rc.1", "-beta1", ...)
//
// Returns ok=false for anything that isn't at least MAJOR.MINOR.PATCH
// with three non-negative integer components — including empty
// strings and placeholders like "dev"/"unknown" that this project uses
// for unversioned dev builds.
func parseSemver(v string) (semver, bool) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimSuffix(v, "-dirty")
	if v == "" {
		return semver{}, false
	}
	// Build metadata MUST be ignored for precedence purposes (SemVer
	// spec item 10) and terminates the version at the first "+".
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var pre string
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" {
			return semver{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}
	sv := semver{major: nums[0], minor: nums[1], patch: nums[2]}
	if pre != "" {
		sv.prerelease = strings.Split(pre, ".")
	}
	return sv, true
}

// compareSemver returns -1, 0, or +1 as a<b, a==b, a>b, using SemVer
// 2.0.0 precedence rules (spec item 11): numeric MAJOR.MINOR.PATCH
// comparison first; when those are equal, a pre-release version has
// *lower* precedence than the associated normal version, and two
// pre-release versions are compared identifier-by-identifier.
func compareSemver(a, b semver) int {
	if a.major != b.major {
		return cmpInt(a.major, b.major)
	}
	if a.minor != b.minor {
		return cmpInt(a.minor, b.minor)
	}
	if a.patch != b.patch {
		return cmpInt(a.patch, b.patch)
	}
	switch {
	case len(a.prerelease) == 0 && len(b.prerelease) == 0:
		return 0
	case len(a.prerelease) == 0:
		return 1 // a is the normal release, b is a pre-release of it: a wins
	case len(b.prerelease) == 0:
		return -1
	}
	n := min(len(a.prerelease), len(b.prerelease))
	for i := range n {
		if c := comparePrereleaseIdentifier(a.prerelease[i], b.prerelease[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers equal: the longer identifier list has
	// higher precedence (spec item 11.4.4).
	return cmpInt(len(a.prerelease), len(b.prerelease))
}

// comparePrereleaseIdentifier compares one dot-separated pre-release
// identifier per spec item 11.4.3: numeric identifiers compare
// numerically, alphanumeric identifiers compare lexically (ASCII), and
// numeric identifiers always have lower precedence than alphanumeric
// ones.
func comparePrereleaseIdentifier(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	aNum, bNum := aErr == nil, bErr == nil
	switch {
	case aNum && bNum:
		return cmpInt(an, bn)
	case aNum && !bNum:
		return -1
	case !aNum && bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// IsUpgrade reports whether candidate is a strictly greater SemVer
// version than current. An unparseable version on either side (empty,
// "dev", "unknown", or any other non-SemVer shape) always returns
// false — we never claim something is an upgrade when we can't
// actually tell.
//
// This is the sole authority behind both the "a newer release is
// available" notification (notify.go) and mk update's downgrade guard
// (see the cli package). Because it is pure numeric SemVer precedence
// with no upper bound and no repository-specific special-casing, a
// client built from any v0.x tag — including a downstream fork that
// has advanced its own patch series ahead of this repository's own
// tags (e.g. a fork at v0.8.6 while this repo is still at v0.2.0) —
// correctly treats a new upstream v0.9.0+ (or v1.0.0+) release as an
// upgrade: SemVer precedence only looks at the three numeric
// components (plus pre-release, if any), never at which repository
// originally minted a given tag.
func IsUpgrade(candidate, current string) bool {
	c, ok := parseSemver(candidate)
	if !ok {
		return false
	}
	cur, ok := parseSemver(current)
	if !ok {
		return false
	}
	return compareSemver(c, cur) > 0
}

// IsDowngrade reports whether target is a strictly lower SemVer
// version than current. Like IsUpgrade, an unparseable version on
// either side returns false: "can't tell" must never block an update
// (a dev/unknown build's own version, or a network hiccup that
// produced a garbage tag, should never wedge `mk update`) — only a
// version comparison we can make with confidence blocks it.
func IsDowngrade(target, current string) bool {
	t, ok := parseSemver(target)
	if !ok {
		return false
	}
	cur, ok := parseSemver(current)
	if !ok {
		return false
	}
	return compareSemver(t, cur) < 0
}
