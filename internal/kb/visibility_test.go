package kb

import "testing"

// visibility_test.go pins the derivation every read surface depends on:
// which page IDs are private, to whom, and what a given viewer may see.

func TestPrivateOwner(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"memory/personal/alice-8f2a1c0b9d8e7f60/deploy-checklist", "alice-8f2a1c0b9d8e7f60"},
		{"memory/personal/local/note", "local"},
		{"/memory/personal/alice-8f2a1c0b9d8e7f60/note", "alice-8f2a1c0b9d8e7f60"},
		// Nested keys keep the FIRST segment as the owner: the store only
		// ever writes one, but a deeper path must not resolve to a
		// different principal.
		{"memory/personal/alice-8f2a1c0b9d8e7f60/a/b/c", "alice-8f2a1c0b9d8e7f60"},
		// Truncated: no document after the namespace. There is no such
		// page, and answering with the owner keeps the failure closed.
		{"memory/personal/alice-8f2a1c0b9d8e7f60", "alice-8f2a1c0b9d8e7f60"},
		// Everything else is public.
		{"memory/team/runbook", ""},
		{"memory/global/policy", ""},
		{"handbook/onboarding", ""},
		{"memory/personal", ""},
		{"memory/personal/", ""},
		{"", ""},
		// A near-miss prefix is not the prefix.
		{"memory/personalx/alice/note", ""},
		{"notmemory/personal/alice/note", ""},
	} {
		if got := PrivateOwner(tc.id); got != tc.want {
			t.Errorf("PrivateOwner(%q) = %q, want %q", tc.id, got, tc.want)
		}
		if got := (Page{ID: tc.id}).PrivateOwner(); got != tc.want {
			t.Errorf("Page{ID: %q}.PrivateOwner() = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// TestPrivateOwner_IgnoresFrontmatter pins that the owner comes from the
// ID and from nothing in the document. A memory's body is caller-written
// and its frontmatter round-trips into Extra, so a page that CLAIMS a
// different owner must not get one.
func TestPrivateOwner_IgnoresFrontmatter(t *testing.T) {
	page, err := ParsePage("memory/personal/alice-1111111111111111/note", "memory/personal/alice/note.md", []byte(
		"---\n"+
			"title: Note\n"+
			"memory_namespace: bob-2222222222222222\n"+
			"memory_scope: global\n"+
			"---\n\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := page.PrivateOwner(); got != "alice-1111111111111111" {
		t.Errorf("PrivateOwner() = %q, want the ID's owner — the frontmatter claimed another", got)
	}
	if !AsOwner("alice-1111111111111111").CanSee(page) {
		t.Error("the real owner cannot see their own page")
	}
	if AsOwner("bob-2222222222222222").CanSee(page) {
		t.Error("the frontmatter's claimed namespace was honoured — it must not be")
	}
}

func TestViewer_CanSee(t *testing.T) {
	alice := Page{ID: "memory/personal/alice-1111111111111111/note"}
	bob := Page{ID: "memory/personal/bob-2222222222222222/note"}
	public := Page{ID: "handbook/onboarding"}
	team := Page{ID: "memory/team/runbook"}

	for _, tc := range []struct {
		name              string
		v                 Viewer
		alice, bob, other bool
	}{
		{"unfiltered sees everything", Unfiltered(), true, true, true},
		{"alice sees her own and the public ones", AsOwner("alice-1111111111111111"), true, false, true},
		{"bob sees his own and the public ones", AsOwner("bob-2222222222222222"), false, true, true},
		{"a caller who owns nothing sees only the public ones", AsOwner(""), false, false, true},
		{"the zero viewer is the least-privileged one", Viewer{}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.CanSee(alice); got != tc.alice {
				t.Errorf("CanSee(alice's) = %v, want %v", got, tc.alice)
			}
			if got := tc.v.CanSee(bob); got != tc.bob {
				t.Errorf("CanSee(bob's) = %v, want %v", got, tc.bob)
			}
			for _, p := range []Page{public, team} {
				if got := tc.v.CanSee(p); got != tc.other {
					t.Errorf("CanSee(%s) = %v, want %v", p.ID, got, tc.other)
				}
			}
		})
	}
}

// TestViewer_UnfilteredIsNotTheSameAsOwningNothing pins the distinction
// the registry's nil-viewer pointer exists to preserve: "no policy in
// force" and "a caller with no identity" must not collapse into one
// value, because they are opposites.
func TestViewer_UnfilteredIsNotTheSameAsOwningNothing(t *testing.T) {
	private := Page{ID: "memory/personal/alice-1111111111111111/note"}
	if !Unfiltered().CanSee(private) {
		t.Error("Unfiltered() cannot see a private page")
	}
	if AsOwner("").CanSee(private) {
		t.Error("AsOwner(\"\") can see a private page")
	}
	if AsOwner("").IsUnfiltered() {
		t.Error("AsOwner(\"\") reports itself unfiltered")
	}
}

func TestViewer_VisiblePages(t *testing.T) {
	pages := []Page{
		{ID: "handbook/onboarding"},
		{ID: "memory/personal/alice-1111111111111111/a"},
		{ID: "memory/personal/bob-2222222222222222/b"},
		{ID: "memory/team/runbook"},
	}

	got := AsOwner("alice-1111111111111111").VisiblePages(pages)
	if len(got) != 3 {
		t.Fatalf("alice sees %d pages, want 3", len(got))
	}
	for _, p := range got {
		if p.ID == "memory/personal/bob-2222222222222222/b" {
			t.Error("alice sees bob's page")
		}
	}
	// An unfiltered viewer gets the input back untouched — no copy, no
	// allocation, no behaviour change for a single-user surface.
	if all := Unfiltered().VisiblePages(pages); len(all) != len(pages) {
		t.Errorf("unfiltered sees %d pages, want %d", len(all), len(pages))
	}
	// The input slice is not modified in place.
	if len(pages) != 4 {
		t.Errorf("VisiblePages mutated its input: %d pages left", len(pages))
	}
}
