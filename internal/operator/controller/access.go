package controller

import (
	"fmt"
	"slices"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

// authFor is the auth: block u's instances load. Shared units get the
// mob's block unchanged. A dedicated unit whose KnowledgeBase lists
// access.identities gets a copy narrowed to those identities, as
// decision D-11 (meerkat-mob#10) sets out:
//
//   - every rule that grants the KnowledgeBase, by name or by "*",
//     keeps only the subjects that are also identities;
//   - a rule with no subjects gets the identities as its subjects;
//   - a rule left with no subject no longer applies, so it is dropped;
//   - an anonymous rule that grants the KnowledgeBase, or
//     allow_unauthenticated, refuses the mob, because neither has a
//     subject to narrow.
//
// Rules only add grants and the selectors of one rule are a
// conjunction, so narrowing the subjects of each granting rule is what
// keeps the KnowledgeBase to its identities: groups, emails, tenant,
// issuer and capabilities stay as written. A dedicated unit serves its
// KnowledgeBase alone (planUnits), so a rule's other collections are
// not mounted there and dropping the whole rule takes nothing else away.
//
// Identities are matched against the token's sub. With more than one
// provider a narrowed rule must carry issuer:, so a sub from another
// provider cannot match (the server refuses subjects without issuer
// then; this says why in terms of the KnowledgeBase).
func authFor(u *unit, auth *authz.Config) (*authz.Config, error) {
	kb := dedicatedKB(u)
	if kb == nil || kb.Spec.Access == nil || len(kb.Spec.Access.Identities) == 0 || auth == nil {
		return auth, nil
	}
	ids, err := identities(kb)
	if err != nil {
		return nil, err
	}
	if auth.AllowUnauthenticated {
		return nil, fmt.Errorf("KnowledgeBase %q lists access.identities, which auth.allow_unauthenticated cannot enforce: "+
			"with it no caller has a subject to match", kb.Name)
	}
	out := *auth
	out.Providers = slices.Clone(auth.Providers)
	out.Rules = make([]authz.Rule, 0, len(auth.Rules))
	for i, r := range auth.Rules {
		if !grants(r, kb.Name) {
			out.Rules = append(out.Rules, r)
			continue
		}
		if r.Anonymous {
			return nil, fmt.Errorf("%s publishes KnowledgeBase %q to anonymous callers, but its access.identities "+
				"restrict it to %d identities; an anonymous caller has no subject to match", authRuleLabel(i, r), kb.Name, len(ids))
		}
		subjects := ids
		if len(r.Subjects) > 0 {
			subjects = nil
			for _, s := range r.Subjects {
				if slices.Contains(ids, s) && !slices.Contains(subjects, s) {
					subjects = append(subjects, s)
				}
			}
		}
		if len(subjects) == 0 {
			continue // no subject of the rule is an identity: it no longer applies to the KnowledgeBase
		}
		if len(auth.Providers) > 1 && r.Issuer == "" {
			return nil, fmt.Errorf("%s grants KnowledgeBase %q, whose access.identities are matched against the token's sub; "+
				"with %d providers configured the rule needs issuer: so a subject from another provider cannot match",
				authRuleLabel(i, r), kb.Name, len(auth.Providers))
		}
		r.Subjects = slices.Clone(subjects)
		r.Collections = slices.Clone(r.Collections)
		r.Capabilities = slices.Clone(r.Capabilities)
		out.Rules = append(out.Rules, r)
	}
	return &out, nil
}

// dedicatedKB is the KnowledgeBase a dedicated unit serves, or nil for
// any other unit.
func dedicatedKB(u *unit) *v1alpha1.KnowledgeBase {
	if len(u.KBs) != 1 || u.KBs[0].Spec.Placement != v1alpha1.PlacementDedicated {
		return nil
	}
	return &u.KBs[0]
}

// identities is kb's access.identities without repeats, refusing an
// empty entry: no token carries an empty sub, so it would read as an
// identity and match nobody.
func identities(kb *v1alpha1.KnowledgeBase) ([]string, error) {
	var out []string
	for _, id := range kb.Spec.Access.Identities {
		if id == "" {
			return nil, fmt.Errorf("KnowledgeBase %q: access.identities contains an empty entry", kb.Name)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// grants reports whether r grants the collection name, by name or "*".
func grants(r authz.Rule, name string) bool {
	return slices.Contains(r.Collections, name) || slices.Contains(r.Collections, "*")
}

// authRuleLabel names rule i of the mob's auth: block in a refusal.
func authRuleLabel(i int, r authz.Rule) string {
	if r.Name != "" {
		return fmt.Sprintf("auth.rules[%s]", r.Name)
	}
	return fmt.Sprintf("auth.rules[%d]", i)
}
