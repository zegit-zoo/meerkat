package memory

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// Outcome is what the policy permits for one save request.
type Outcome int

const (
	// OutcomeRefused: the caller may not write this scope here at all.
	OutcomeRefused Outcome = iota
	// OutcomeWrite: write the memory directly; it becomes live and
	// searchable.
	OutcomeWrite
	// OutcomeStage: the caller is a writer here but not for this scope,
	// so the memory becomes a pending review artifact instead.
	OutcomeStage
)

// Decision is Authorize's answer: what to do, whose memory it is, and —
// when refused — why, in a sentence that names no collection the caller
// cannot already see.
type Decision struct {
	Outcome Outcome
	// Identity is the VERIFIED caller. It is returned by Authorize
	// rather than passed in, so there is no call site at which a
	// different identity could be supplied.
	Identity authz.Identity
	// Namespace is derived from Identity and from nothing else.
	Namespace string
	// Reason explains OutcomeRefused.
	Reason string
}

// ErrRefused is returned by Authorize when the caller may not write.
var ErrRefused = errors.New("memory write refused")

// Authorize decides what happens to a save request, from the grants in
// force and the scope asked for. It is the whole authorization model
// for writes:
//
//	scope     capability required   held?          not held
//	personal  personal-write        write          refuse
//	team      team-write            write          stage, if any write capability
//	global    global-write          write          stage, if any write capability
//
// Personal has no staging row on purpose: a personal memory has no
// reviewer — it is nobody's business but the caller's — so an
// unauthorized personal write is simply refused rather than parked
// somewhere a stranger would have to triage.
//
// The caller's identity and namespace come from g, never from an
// argument. There is deliberately no parameter by which a caller could
// name a subject, a namespace, or a scope owner: a crafted `key`,
// `collection` or `scope` argument changes WHERE within the caller's
// own space a memory lands, never WHOSE space that is.
//
// allowAnonymousPersonal permits personal writes for a caller with no
// verified subject. It is true only on the stdio transport, which was
// spawned by the single user it serves; on the hosted transport an
// anonymous personal write is refused, because every anonymous caller
// would otherwise share one namespace.
func Authorize(g *authz.Grants, collection string, scope Scope, allowAnonymousPersonal bool) (Decision, error) {
	id := g.Identity()
	d := Decision{Identity: id, Namespace: Namespace(id)}

	if scope == ScopePersonal && Anonymous(id) && !allowAnonymousPersonal {
		d.Reason = "a personal memory needs a verified identity: this request carries no authenticated subject, " +
			"and every unauthenticated caller would otherwise share one namespace"
		return d, fmt.Errorf("%w: %s", ErrRefused, d.Reason)
	}

	if g.Can(collection, scope.Capability()) {
		d.Outcome = OutcomeWrite
		return d, nil
	}

	if scope != ScopePersonal && g.CanWrite(collection) {
		d.Outcome = OutcomeStage
		return d, nil
	}

	d.Reason = refusalReason(g, collection, scope)
	return d, fmt.Errorf("%w: %s", ErrRefused, d.Reason)
}

// refusalReason explains a refusal in terms of the capability the
// caller lacks and the ones they hold.
//
// It names the collection — which is safe, because the write path only
// ever reaches here for a collection the caller was explicitly granted
// a write capability on (see internal/mcp's writable view, which
// restricts on CanWrite). A collection they hold nothing over never
// gets this far: it is not in their registry at all, and resolving it
// fails with the same unknown-collection sentence a name nobody mounted
// produces.
//
// The "holds none there" branch is therefore unreachable through MCP.
// It is written anyway because Authorize is a public function of this
// package and must answer sensibly for any grants it is handed.
func refusalReason(g *authz.Grants, collection string, scope Scope) string {
	held := g.Capabilities(collection).List()
	var b strings.Builder
	fmt.Fprintf(&b, "saving a %s memory to collection %q needs the %q capability",
		scope, collection, scope.Capability())
	if len(held) == 0 {
		b.WriteString("; this identity holds none there")
		return b.String()
	}
	names := make([]string, 0, len(held))
	for _, c := range held {
		names = append(names, string(c))
	}
	fmt.Fprintf(&b, "; this identity holds %s there", strings.Join(names, ", "))
	if scope != ScopePersonal {
		b.WriteString(" — none of which permits writing or proposing at this scope")
	}
	return b.String()
}
