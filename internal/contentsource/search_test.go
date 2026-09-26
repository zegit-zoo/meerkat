package contentsource

import (
	"context"
	"strings"
	"testing"
)

// search_test.go covers a collection's `search:` block (issue #88): the
// per-collection type_boosts that let a LEAF collection opt out of the
// hub-shaped defaults without touching any other collection.

func TestParseConfig_SearchTypeBoosts(t *testing.T) {
	const head = "collections:\n  - name: mk-mpe\n    type: local\n    path: ./kb\n"
	for _, tc := range []struct {
		name    string
		block   string
		wantNil bool // Search.TypeBoosts is nil: keep the defaults
		want    map[string]float64
	}{
		{name: "absent keeps the defaults", block: "", wantNil: true},
		{name: "a search block without the key keeps the defaults", block: "    search: {}\n", wantNil: true},
		{name: "null keeps the defaults", block: "    search:\n      type_boosts:\n", wantNil: true},
		{name: "an empty map is boosting off, not the defaults", block: "    search:\n      type_boosts: {}\n", want: map[string]float64{}},
		{name: "a leaf collection unboosts pointers", block: "    search:\n      type_boosts: {pointer: 1.0}\n", want: map[string]float64{"pointer": 1}},
		{name: "a type no page carries yet is accepted", block: "    search:\n      type_boosts: {runbook: 3, skill: 0.5}\n", want: map[string]float64{"runbook": 3, "skill": 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig([]byte(head+tc.block), "c.yaml")
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			var got map[string]float64
			if s := cfg.Collections[0].Search; s != nil {
				got = s.TypeBoosts
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("type_boosts = %v, want nil (defaults)", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("type_boosts is nil, want %v — an explicit map must survive parsing, even an empty one", tc.want)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("type_boosts = %v, want %v", got, tc.want)
			}
			for k, w := range tc.want {
				if got[k] != w {
					t.Errorf("type_boosts[%s] = %v, want %v", k, got[k], w)
				}
			}
		})
	}
}

func TestParseConfig_SearchTypeBoostsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "a negative weight",
			yaml: "collections:\n  - name: mk-mpe\n    type: local\n    path: ./kb\n    search:\n      type_boosts: {pointer: -1}\n",
			want: "collections[mk-mpe].search.type_boosts[pointer] must be a positive number",
		},
		{
			name: "zero, which would silently mean x1",
			yaml: "collections:\n  - name: mk-mpe\n    type: local\n    path: ./kb\n    search:\n      type_boosts: {pointer: 0}\n",
			want: "collections[mk-mpe].search.type_boosts[pointer] must be a positive number (1 leaves the type unboosted), got 0",
		},
		{
			name: "NaN",
			yaml: "collections:\n  - name: mk-mpe\n    type: local\n    path: ./kb\n    search:\n      type_boosts: {skill: .nan}\n",
			want: "search.type_boosts[skill]",
		},
		{
			name: "infinity",
			yaml: "collections:\n  - name: mk-mpe\n    type: local\n    path: ./kb\n    search:\n      type_boosts: {skill: .inf}\n",
			want: "search.type_boosts[skill]",
		},
		{
			name: "an empty type name",
			yaml: "collections:\n  - name: mk-mpe\n    type: local\n    path: ./kb\n    search:\n      type_boosts: {\"\": 2}\n",
			want: "collections[mk-mpe].search.type_boosts has an empty type name",
		},
		{
			name: "the embedded source validates it too",
			yaml: "content:\n  type: none\n  search:\n    type_boosts: {pointer: -2}\n",
			want: "content.search.type_boosts[pointer]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.yaml), "c.yaml")
			if err == nil {
				t.Fatalf("parseConfig accepted it; want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The resolved collection is what internal/collections builds each index
// from, so the block has to arrive there intact — an empty map included.
func TestResolveRuntimeCollections_CarriesTheSearchBlockThrough(t *testing.T) {
	path := writeCfg(t, `
content:
  type: none
  search:
    type_boosts: {}
`)
	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 1 {
		t.Fatalf("got %d collections, want 1", len(cols))
	}
	s := cols[0].Source.Search
	if s == nil || s.TypeBoosts == nil || len(s.TypeBoosts) != 0 {
		t.Fatalf("search = %+v, want an explicit empty type_boosts map", s)
	}
}
