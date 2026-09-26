package contentsource

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// SearchSpec is a collection's `search:` block: how ITS pages are
// ranked. Absent (the default, and what every configuration written
// before it existed has) the collection ranks exactly as it always did.
//
// It sits beside layout: rather than inside it because layout: says
// where artifacts live in the resolved source, while this says how the
// served pages compete for a query. layout.analyzer predates the block
// and stays where it is.
type SearchSpec struct {
	// TypeBoosts replaces search.DefaultTypeBoosts for this collection
	// only: a hit whose frontmatter `type` is a key has its final score
	// multiplied by the weight.
	//
	// Three readings, and the difference between the first two matters:
	//
	//	type_boosts absent or null   the defaults (pointer ×4, skill ×2, example ×1.5)
	//	type_boosts: {}              no type boosting at all
	//	type_boosts: {pointer: 1.0}  exactly this map; unlisted types are unboosted
	//
	// The defaults assume a HUB: a small set of routing pages above thin
	// content, where "the pointer outranks the page" is the point. A LEAF
	// collection that cites its sources through pointers — one per book
	// chapter or article — is the opposite shape, and there the ×4 lets a
	// citation outrank the page that answers the question (issue #88).
	//
	// Keys are not checked against the known types: `type` is open-ended
	// frontmatter, and a boost for a type no page carries yet is inert,
	// not wrong. Weights must be positive and finite — see validate.
	TypeBoosts map[string]float64 `yaml:"type_boosts"`
}

// validate checks a `search:` block. p is the parent path ("content" or
// "collections[docs]"), so a message names the exact key to fix.
//
// A weight must be > 0 and finite. Negative is meaningless for a
// multiplier. Zero is refused too, rather than accepted: it reads as
// "hide this type", but the ranker treats a non-positive weight as no
// boost at all, so a 0 would silently mean ×1. An operator who wants a
// type unboosted writes 1, or leaves it out of the map.
func (s *SearchSpec) validate(p string) error {
	if s == nil {
		return nil
	}
	types := make([]string, 0, len(s.TypeBoosts))
	for typ := range s.TypeBoosts {
		types = append(types, typ)
	}
	slices.Sort(types) // deterministic: the first offending key, alphabetically
	for _, typ := range types {
		w := s.TypeBoosts[typ]
		if strings.TrimSpace(typ) == "" {
			return fmt.Errorf("%s.search.type_boosts has an empty type name", p)
		}
		if math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 {
			return fmt.Errorf("%s.search.type_boosts[%s] must be a positive number (1 leaves the type unboosted), got %v", p, typ, w)
		}
	}
	return nil
}
