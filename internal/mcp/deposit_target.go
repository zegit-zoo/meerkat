package mcp

import (
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/intake"
)

// deposit_validate.go holds the checks mk_report_outcome applies to what
// a caller deposits for the intake pipeline, kept apart from the tool's
// argument plumbing in outcome.go.

// unroutedTarget is the deposit target when no attempted collection
// qualifies: the librarian places the candidate.
const unroutedTarget = "unrouted"

// depositArgs is the report as it is written to the intake store: the
// attempted list cut down to the collections this caller can see, each
// as its collection name (meerkat-mob#38). The pipeline files a
// candidate into, and escalates to the forge of, the deposit's target,
// so a caller must not be able to name a collection outside their own
// view. The traversal log keeps the list as reported (it is hashed and
// only matched against the registry's own names).
func depositArgs(g *authz.Grants, view *collections.Registry, a outcomeArgs) outcomeArgs {
	var kept []string
	for _, name := range a.attempted {
		c, err := view.Get(name)
		if err != nil || !depositTargetAllowed(g, c.Name) {
			continue
		}
		kept = append(kept, c.Name)
	}
	a.attempted = kept
	return a
}

// depositTargetAllowed is the depositor-to-target rule: the caller holds
// intake-write on the collection, or can at least read it (an agent
// that searched a collection and gave up is the intended reporter).
// A name that cannot be one intake key segment never qualifies.
func depositTargetAllowed(g *authz.Grants, name string) bool {
	if intake.Segment(name) != nil {
		return false
	}
	return g.Can(name, authz.CapIntakeWrite) || g.CanRead(name)
}

// depositTarget is the target a written deposit records: the deepest
// (last) collection kept by depositArgs, else unrouted.
func depositTarget(attempted []string) string {
	if n := len(attempted); n > 0 {
		return attempted[n-1]
	}
	return unroutedTarget
}
