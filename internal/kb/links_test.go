package kb

import (
	"strings"
	"testing"
)

func TestParseLink(t *testing.T) {
	cases := []struct {
		raw  string
		want Link
		err  string
	}{
		{"systems/backend/api", Link{Kind: LinkLocal, ID: "systems/backend/api"}, ""},
		{"/systems/api.md", Link{Kind: LinkLocal, ID: "systems/api"}, ""},
		{"flux:helmrelease-drift", Link{Kind: LinkQualified, Collection: "flux", ID: "helmrelease-drift"}, ""},
		{"page:flux:concepts/drift", Link{Kind: LinkQualified, Collection: "flux", ID: "concepts/drift"}, ""},
		{"collection:flux", Link{Kind: LinkCollection, Collection: "flux"}, ""},
		{"ext:mcp:datadog", Link{Kind: LinkExternal, Target: "mcp:datadog"}, ""},
		{"external:datadog", Link{Kind: LinkExternal, Target: "datadog"}, ""},
		{"mcp://datadog", Link{Kind: LinkExternal, Target: "mcp://datadog"}, ""},
		{"https://example.com/doc", Link{Kind: LinkExternal, Target: "https://example.com/doc"}, ""},
		{"", Link{}, "empty"},
		{"has space", Link{}, "whitespace"},
		{"ext:datadog", Link{}, "ext:<scheme>:<target>"},
		{"external:", Link{}, "names nothing"},
		{"collection:not valid", Link{}, "whitespace"},
		{"collection:-bad", Link{}, "not a valid collection name"},
		{"page:flux", Link{}, "page:<collection>:<id>"},
		{"a:b:c", Link{}, "expected <collection>:<id>"},
		{"../escape", Link{}, "not a page id"},
		{"bad-coll!:id", Link{}, "expected <collection>:<id>"},
	}
	for _, c := range cases {
		got, err := ParseLink(c.raw)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("ParseLink(%q): err = %v, want containing %q", c.raw, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLink(%q): %v", c.raw, err)
			continue
		}
		c.want.Raw = c.raw
		if got != c.want {
			t.Errorf("ParseLink(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

func TestLinkString(t *testing.T) {
	for _, raw := range []string{"a/b", "flux:a/b", "collection:flux", "mcp://datadog"} {
		l, err := ParseLink(raw)
		if err != nil {
			t.Fatal(err)
		}
		if l.String() != raw {
			t.Errorf("String(%q) = %q", raw, l.String())
		}
	}
	if l, _ := ParseLink("page:flux:x"); l.String() != "flux:x" {
		t.Errorf("page: form canonicalises to <collection>:<id>, got %q", l.String())
	}
}

func TestParseRelated_KeepsGoodLinksBesideBadOnes(t *testing.T) {
	links, errs := ParseRelated([]string{"good", "", "flux:ok", "bad link"})
	if len(links) != 2 || len(errs) != 2 {
		t.Fatalf("links=%v errs=%v", links, errs)
	}
}

func TestPointer(t *testing.T) {
	ok := Page{ID: "hub/flux", Front: Frontmatter{Type: TypePointer, Target: "collection:flux", Hint: "Flux CD, GitOps, HelmRelease drift."}}
	l, err := ok.Pointer()
	if err != nil || l.Kind != LinkCollection || l.Collection != "flux" {
		t.Fatalf("Pointer = %+v %v", l, err)
	}
	ext := ok
	ext.Front.Target = "mcp://datadog"
	if l, err := ext.Pointer(); err != nil || l.Kind != LinkExternal {
		t.Errorf("external pointer = %+v %v", l, err)
	}
	page := ok
	page.Front.Target = "flux:concepts/drift"
	if l, err := page.Pointer(); err != nil || l.Kind != LinkQualified {
		t.Errorf("page pointer = %+v %v", l, err)
	}

	bad := []struct {
		name string
		mut  func(*Page)
		want string
	}{
		{"not a pointer", func(p *Page) { p.Front.Type = "Metric" }, "not \"pointer\""},
		{"no target", func(p *Page) { p.Front.Target = "" }, "no target"},
		{"local target", func(p *Page) { p.Front.Target = "some/page" }, "use related:"},
		{"bad target", func(p *Page) { p.Front.Target = "ext:x" }, "ext:<scheme>:<target>"},
		{"no hint", func(p *Page) { p.Front.Hint = " " }, "no hint"},
		{"long hint", func(p *Page) { p.Front.Hint = strings.Repeat("x", maxHintLen+1) }, "over the"},
	}
	for _, c := range bad {
		p := ok
		c.mut(&p)
		if _, err := p.Pointer(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestFrontmatter_RoutingFieldsRoundTrip(t *testing.T) {
	body := "---\ntype: pointer\ntarget: mcp://datadog\nhint: Datadog's own docs and skills.\ntools: [search_datadog_docs]\nresources: [datadog://runbooks]\nprompts: [triage]\ntoolsets: [monitoring]\nrelated: [flux:drift, ext:mcp:pagerduty]\n---\n# Datadog\n"
	p, err := ParsePage("hub/datadog", "content/hub/datadog.md", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if p.Front.Target != "mcp://datadog" || p.Front.Hint == "" || len(p.Front.Tools) != 1 || len(p.Front.Resources) != 1 || len(p.Front.Prompts) != 1 || len(p.Front.Toolsets) != 1 {
		t.Errorf("routing fields not parsed: %+v", p.Front)
	}
	if _, has := p.Front.Extra["target"]; has {
		t.Error("target must be a core key, not spill into extra")
	}
	links, errs := p.Links()
	if len(errs) != 0 || len(links) != 2 {
		t.Errorf("Links = %v %v", links, errs)
	}
}
