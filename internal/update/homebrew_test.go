package update

import "testing"

// TestIsHomebrewInstall covers the three standard Homebrew prefixes,
// the user-owned install locations that must stay self-updatable, and
// the near-miss paths where "Cellar" appears without being a directory
// component of its own.
func TestIsHomebrewInstall(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		want bool
	}{
		{
			name: "apple silicon cellar",
			exe:  "/opt/homebrew/Cellar/meerkat/0.11.1/bin/meerkat",
			want: true,
		},
		{
			name: "intel mac cellar",
			exe:  "/usr/local/Cellar/meerkat/0.11.1/bin/meerkat",
			want: true,
		},
		{
			name: "linuxbrew cellar",
			exe:  "/home/linuxbrew/.linuxbrew/Cellar/meerkat/0.11.1/bin/meerkat",
			want: true,
		},
		{
			name: "relocated prefix cellar",
			exe:  "/srv/brew/Cellar/meerkat/0.11.1/bin/meerkat",
			want: true,
		},
		{
			name: "unclean path still matches",
			exe:  "/opt/homebrew/Cellar/meerkat/0.11.1/bin/../bin/meerkat",
			want: true,
		},
		{
			name: "user local bin",
			exe:  "/Users/someone/.local/bin/meerkat",
			want: false,
		},
		{
			name: "homebrew bin symlink target outside cellar",
			// $HOMEBREW_PREFIX/bin itself is not a Cellar path; only the
			// symlink's *target* is, which is why callers resolve first.
			exe:  "/opt/homebrew/bin/meerkat",
			want: false,
		},
		{
			name: "usr local bin (tarball install, not brew)",
			exe:  "/usr/local/bin/meerkat",
			want: false,
		},
		{
			name: "cellar as a filename substring",
			exe:  "/home/someone/bin/Cellar-backup-meerkat",
			want: false,
		},
		{
			name: "cellar as a component substring",
			exe:  "/srv/CellarKeeper/bin/meerkat",
			want: false,
		},
		{
			name: "empty path",
			exe:  "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Make sure an inherited HOMEBREW_PREFIX from the developer's
			// own shell can't decide these cases.
			t.Setenv("HOMEBREW_PREFIX", "")
			if got := IsHomebrewInstall(tc.exe); got != tc.want {
				t.Errorf("IsHomebrewInstall(%q) = %v, want %v", tc.exe, got, tc.want)
			}
		})
	}
}

// TestIsHomebrewInstall_HomebrewPrefix exercises the second signal:
// $HOMEBREW_PREFIX/Cellar. The "Cellar" component check already covers
// every real install, so this mostly guards against the env var
// widening the match to things it shouldn't.
func TestIsHomebrewInstall_HomebrewPrefix(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		exe    string
		want   bool
	}{
		{
			name:   "under the configured prefix cellar",
			prefix: "/opt/homebrew",
			exe:    "/opt/homebrew/Cellar/meerkat/0.11.1/bin/meerkat",
			want:   true,
		},
		{
			name:   "prefix set but binary elsewhere",
			prefix: "/opt/homebrew",
			exe:    "/Users/someone/.local/bin/meerkat",
			want:   false,
		},
		{
			name:   "prefix bin dir is not the cellar",
			prefix: "/opt/homebrew",
			exe:    "/opt/homebrew/bin/meerkat",
			want:   false,
		},
		{
			name:   "sibling prefix with a shared name prefix",
			prefix: "/opt/homebrew",
			exe:    "/opt/homebrewXX/lib/meerkat",
			want:   false,
		},
		{
			name:   "whitespace-only prefix is ignored",
			prefix: "   ",
			exe:    "/Users/someone/.local/bin/meerkat",
			want:   false,
		},
		{
			name:   "trailing slash on the prefix",
			prefix: "/opt/homebrew/",
			exe:    "/opt/homebrew/Cellar/meerkat/0.11.1/bin/meerkat",
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOMEBREW_PREFIX", tc.prefix)
			if got := IsHomebrewInstall(tc.exe); got != tc.want {
				t.Errorf("HOMEBREW_PREFIX=%q IsHomebrewInstall(%q) = %v, want %v",
					tc.prefix, tc.exe, got, tc.want)
			}
		})
	}
}

// TestRunningFromHomebrew_TestBinaryIsNotBrew: the test binary lives in
// a go-build temp dir, so the running-executable wrapper must answer
// "no" — and, crucially, must not panic or error out on a path that
// EvalSymlinks can resolve perfectly well.
func TestRunningFromHomebrew_TestBinaryIsNotBrew(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "")
	if RunningFromHomebrew() {
		t.Error("expected the go test binary not to look like a Homebrew install")
	}
}
