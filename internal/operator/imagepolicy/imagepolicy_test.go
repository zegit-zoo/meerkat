package imagepolicy

import (
	"strings"
	"testing"
)

func TestNew_RefusesUnqualifiedOrTaggedEntries(t *testing.T) {
	for _, a := range []string{"meerkat", "zegit-zoo/meerkat", "ghcr.io/zegit-zoo/meerkat:1.0", "ghcr.io/zegit-zoo/meerkat@sha256:00", "ghcr.io/x y"} {
		if _, err := New("ghcr.io/zegit-zoo/meerkat:1.0", []string{a}); err == nil {
			t.Errorf("allowed entry %q was accepted", a)
		}
	}
	for _, a := range []string{"ghcr.io/zegit-zoo/meerkat", "registry.local:5000/meerkat", "localhost/meerkat", "registry.example.net/mirror/", " "} {
		if _, err := New("ghcr.io/zegit-zoo/meerkat:1.0", []string{a}); err != nil {
			t.Errorf("allowed entry %q: %v", a, err)
		}
	}
	if _, err := New("", nil); err == nil {
		t.Error("an empty default image was accepted")
	}
	if _, err := New("ghcr.io//meerkat", nil); err == nil {
		t.Error("a default image with an empty segment was accepted")
	}
}

func TestResolve(t *testing.T) {
	p, err := New("ghcr.io/zegit-zoo/meerkat:0.21.0", []string{DefaultRepository, "registry.local:5000/mirror/"})
	if err != nil {
		t.Fatal(err)
	}
	for image, want := range map[string]string{
		"":                                      "ghcr.io/zegit-zoo/meerkat:0.21.0",
		"ghcr.io/zegit-zoo/meerkat:0.20.0":      "ghcr.io/zegit-zoo/meerkat:0.20.0",
		"ghcr.io/zegit-zoo/meerkat":             "ghcr.io/zegit-zoo/meerkat",
		"ghcr.io/zegit-zoo/meerkat@sha256:abcd": "ghcr.io/zegit-zoo/meerkat@sha256:abcd",
		"ghcr.io/zegit-zoo/meerkat:1@sha256:abcd":  "ghcr.io/zegit-zoo/meerkat:1@sha256:abcd",
		"registry.local:5000/mirror/meerkat:1":     "registry.local:5000/mirror/meerkat:1",
		"registry.local:5000/mirror/sub/meerkat:1": "registry.local:5000/mirror/sub/meerkat:1",
	} {
		got, err := p.Resolve(image)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", image, got, err, want)
		}
	}
	for _, image := range []string{
		"ghcr.io/zegit-zoo/meerkat-evil:1",     // a repository that only starts with the allowed one
		"ghcr.io/zegit-zoo/meerkatx",           // likewise, without a tag
		"ghcr.io/zegit-zoo/meerkat/sub:1",      // below an exact entry
		"ghcr.io/attacker/meerkat:1",           // another owner
		"meerkat:1",                            // docker.io/library/meerkat
		"registry.local:5000/mirror2/meerkat",  // a sibling of a prefix entry
		"registry.local:5000/mirror/../x:1",    // a dot segment
		"registry.local:5000/mirror//x:1",      // an empty segment
		"https://ghcr.io/zegit-zoo/meerkat:1",  // a scheme
		"ghcr.io/zegit-zoo/meerkat:1 --foo",    // whitespace
		"registry.local:5000/mirror:1",         // the prefix itself, tagged
		"registry.local:5000/mirror@sha256:aa", // the prefix itself, by digest
	} {
		if got, err := p.Resolve(image); err == nil {
			t.Errorf("Resolve(%q) = %q, want a refusal", image, got)
		}
	}
}

func TestResolve_NoOverridesAllowed(t *testing.T) {
	p, err := New("ghcr.io/zegit-zoo/meerkat:0.21.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.Resolve("ghcr.io/zegit-zoo/meerkat:0.21.0"); err != nil || got != "ghcr.io/zegit-zoo/meerkat:0.21.0" {
		t.Errorf("the default itself: %q, %v", got, err)
	}
	_, err = p.Resolve("ghcr.io/zegit-zoo/meerkat:0.20.0")
	if err == nil || !strings.Contains(err.Error(), "allows no image overrides") {
		t.Errorf("err = %v, want the no-overrides refusal", err)
	}
}
