package collections

import (
	"fmt"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
)

// contract.go renders a collection's UPDATE CONTRACT — the operator's
// declaration of how knowledge flows back into it — for one caller.
//
// Two layers, deliberately separate:
//
//   - Contract is what the OPERATOR DECLARED, verbatim from
//     content-source.yaml (see contentsource.UpdateSpec). It is the same
//     for everybody.
//   - EffectiveContract is what THIS caller should actually do, given
//     the capabilities they hold. It is never more permissive than the
//     declared contract — capabilities can only walk a caller DOWN the
//     ladder from a direct write to a proposal, never up.
//
// Both hang off *Collection and *Registry, so the restricted view
// Registry.Restrict produces is the only way to reach either. A
// collection a caller may not read is invisible here exactly as it is
// everywhere else: it is not in their registry, so its contract — and
// the contribution repo URL inside it — cannot be enumerated, named or
// probed for. There is deliberately no package-level function that takes
// a collection name and returns a contract; adding one would reintroduce
// the enumeration oracle Restrict exists to close.
//
// See docs/design/update-contract.md.

// Method is a contribution path.
type Method string

const (
	// MethodNone: no sanctioned contribution path. Either the operator
	// declared none, or this caller can take none of the ones declared.
	MethodNone Method = contentsource.UpdateNone
	// MethodDirect: write into the collection's own backend; the write
	// is the update.
	MethodDirect Method = contentsource.UpdateDirect
	// MethodMergeRequest: open a merge/pull request against the
	// contribution repo and let a human review it.
	MethodMergeRequest Method = contentsource.UpdateMergeRequest
	// MethodStaging is EFFECTIVE-ONLY: no operator ever declares it. It
	// is where a caller lands who may write in the collection but not
	// publish to it — the memory toolset's staging path, which parks the
	// document as a pending review artifact under the collection's
	// memory store (see internal/memory and docs/design/memory.md).
	MethodStaging Method = "staging"
)

// PublishCapability is the capability a caller must hold to be told to
// contribute DIRECTLY to a collection that declares method: direct.
//
// It is global-write, and it is global-write for the same reason the
// memory toolset requires it for a global memory: a direct update to a
// collection's content is immediately visible to every reader of that
// collection. A caller who holds only personal-write is a writer, not a
// publisher, and telling them to write into the knowledge base directly
// would be advice that either fails or — worse — succeeds. admin implies
// it, as admin implies everything.
const PublishCapability = authz.CapGlobalWrite

// Contract is a collection's DECLARED update contract: what the operator
// wrote in content-source.yaml, with defaults applied. Identical for
// every caller.
type Contract struct {
	// Method is the declared path: none | direct | merge-request.
	Method Method `json:"method"`
	// Repo, Host, Branch and Path describe the CONTRIBUTION repo of a
	// merge-request contract — an address that is deliberately not
	// assumed to be the one the collection is served from.
	Repo   string `json:"repo,omitempty"`
	Host   string `json:"host,omitempty"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
	// Instructions is the operator's agent-facing prose: fork-vs-branch
	// policy, page format, what to run before proposing.
	Instructions string `json:"instructions,omitempty"`
}

// Declared reports whether the operator declared a contribution path at
// all.
func (c Contract) Declared() bool { return c.Method != "" && c.Method != MethodNone }

// EffectiveContract is the contract as it applies to ONE caller: the
// path they should actually take, why, and the declared contract it was
// derived from.
//
// Declared is carried alongside Method on purpose. "You may open a merge
// request" and "you may open a merge request because you cannot write
// here directly" are different messages, and an agent that can see both
// values can tell a user which one it is looking at.
type EffectiveContract struct {
	// Collection is the collection the contract belongs to.
	Collection string `json:"collection"`
	// Description is the operator's human/agent context for the
	// collection, when one was configured.
	Description string `json:"description,omitempty"`
	// Method is what this caller should do.
	Method Method `json:"method"`
	// Declared is what the operator declared, before capabilities were
	// taken into account.
	Declared Method `json:"declared_method"`
	// Repo, Host, Branch, Path and Instructions are carried only when
	// they apply to Method — a caller told to stage a proposal has no
	// use for a merge request's target branch.
	Repo         string `json:"repo,omitempty"`
	Host         string `json:"host,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Path         string `json:"path,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	// Reason explains why this caller got this Method, in a sentence
	// that names nothing the caller cannot already see: this collection,
	// and the capabilities they themselves hold.
	Reason string `json:"reason"`
}

// Actionable reports whether the effective contract gives the caller
// something to do.
func (e EffectiveContract) Actionable() bool { return e.Method != "" && e.Method != MethodNone }

// Description returns the operator's description of the collection, or
// "" when none was configured.
func (c *Collection) Description() string { return c.Source.Description }

// Contract returns the collection's DECLARED update contract. It is
// operator-declared and never inferred: a collection with no `update:`
// block reports MethodNone rather than a guess derived from how its
// content happens to be served.
func (c *Collection) Contract() Contract {
	u := c.Source.Update
	switch u.DeclaredMethod() {
	case contentsource.UpdateDirect:
		return Contract{Method: MethodDirect, Instructions: u.Instructions}
	case contentsource.UpdateMergeRequest:
		out := Contract{
			Method:       MethodMergeRequest,
			Repo:         u.Repo,
			Host:         u.Host,
			Branch:       u.Branch,
			Path:         u.Path,
			Instructions: u.Instructions,
		}
		// Defaults are applied at config load (UpdateSpec.Normalize).
		// Re-applied here so a Contract built from a hand-assembled
		// Source — a test, or a caller that resolved a config some other
		// way — still names a branch and a host rather than an empty
		// string an agent would have to interpret.
		if out.Branch == "" {
			out.Branch = contentsource.DefaultUpdateBranch
		}
		if out.Host == "" {
			out.Host = contentsource.UpdateHostOther
		}
		return out
	default:
		return Contract{Method: MethodNone}
	}
}

// EffectiveContract renders the declared contract for one caller: what
// THEY should do, and why.
//
// The ladder, in order, stopping at the first rung that applies:
//
//	direct         declared direct AND the caller holds PublishCapability
//	merge-request  declared merge-request — no capability gates it, because
//	               meerkat is not the authority for it; the forge is
//	staging        a declared path the caller cannot take, a memory store
//	               on the collection, and any write capability held
//	none           everything else, with a reason saying which
//
// A nil *Grants means no policy is in force — stdio MCP, the CLI, the
// static-token HTTP server, a hosted server with no auth: block. There
// is no capability gate to fail in that state (authz.Grants.Can answers
// true for everything), so the effective contract is the DECLARED
// contract, unchanged. That is the local-mode answer and it falls out of
// the same code path rather than being special-cased.
func (c *Collection) EffectiveContract(g *authz.Grants) EffectiveContract {
	declared := c.Contract()
	eff := EffectiveContract{
		Collection:  c.Name,
		Description: c.Source.Description,
		Method:      MethodNone,
		Declared:    declared.Method,
	}

	// Rung 1: a direct write, for a caller who may publish here.
	if declared.Method == MethodDirect && g.Can(c.Name, PublishCapability) {
		eff.Method = MethodDirect
		eff.Instructions = declared.Instructions
		eff.Reason = fmt.Sprintf("the operator declared direct writes as the contribution path for %q, "+
			"and this identity holds %q there", c.Name, PublishCapability)
		return eff
	}

	// Rung 2: a merge request. Declared is sufficient: opening one uses
	// the contributor's own forge credentials against a repo meerkat
	// does not serve, so there is no meerkat capability to check — and a
	// caller who cannot see the collection at all never reaches here,
	// because it is not in their registry.
	if declared.Method == MethodMergeRequest {
		eff.Method = MethodMergeRequest
		eff.Repo, eff.Host, eff.Branch, eff.Path = declared.Repo, declared.Host, declared.Branch, declared.Path
		eff.Instructions = declared.Instructions
		eff.Reason = fmt.Sprintf("the operator declared a review flow for %q: contributions arrive as a merge request against %s, never as a direct write",
			c.Name, declared.Repo)
		return eff
	}

	// Rung 3: propose it through the memory store's staging path. Only
	// as a fallback from a contract the caller cannot take: a collection
	// that declares nothing has had "there is no contribution path here"
	// said about it deliberately, and answering "well, stage it anyway"
	// would render a contract the operator did not write.
	if declared.Declared() && c.Memory() != nil && g.CanWrite(c.Name) {
		eff.Method = MethodStaging
		eff.Instructions = declared.Instructions
		eff.Reason = fmt.Sprintf("%s — this identity may write in %q but not publish to it, "+
			"so propose the change with mk_save_memory: it is stored as a pending review artifact rather than published",
			missingPublishCapability(g, c.Name), c.Name)
		return eff
	}

	// Rung 4: nothing this caller can do.
	eff.Reason = noContractReason(g, c, declared)
	return eff
}

// missingPublishCapability phrases the capability shortfall behind a
// downgrade. It names this collection and the caller's own capabilities
// over it, and nothing else — the same disclosure the memory toolset's
// refusal already makes to a caller who can see the collection.
func missingPublishCapability(g *authz.Grants, collection string) string {
	held := g.Capabilities(collection).List()
	if len(held) == 0 {
		return fmt.Sprintf("a direct write to %q needs the %q capability, which this identity does not hold there", collection, PublishCapability)
	}
	names := make([]string, 0, len(held))
	for _, capability := range held {
		names = append(names, string(capability))
	}
	return fmt.Sprintf("a direct write to %q needs the %q capability; this identity holds %s there",
		collection, PublishCapability, strings.Join(names, ", "))
}

// noContractReason explains an effective MethodNone: either the operator
// declared no path, or the caller can take none of the ones declared.
func noContractReason(g *authz.Grants, c *Collection, declared Contract) string {
	if !declared.Declared() {
		return fmt.Sprintf("the operator declared no update contract for %q", c.Name)
	}
	reason := missingPublishCapability(g, c.Name)
	if c.Memory() == nil {
		return reason + fmt.Sprintf(", and %q has no memory store to stage a proposal in and declares no merge-request path", c.Name)
	}
	return reason + fmt.Sprintf(", and staging a proposal in %q needs a write capability there", c.Name)
}

// EffectiveContracts returns the effective update contract for every
// collection in THIS registry, in configuration order.
//
// "this registry" is the whole access control story: on a view produced
// by Restrict, the loop below runs over the caller's visible collections
// and there is no branch that could reach another one. A hidden
// collection's contract — including its contribution repo URL — is not
// omitted from the result; it was never a candidate for it.
func (r *Registry) EffectiveContracts(g *authz.Grants) []EffectiveContract {
	out := make([]EffectiveContract, 0, len(r.list))
	for _, c := range r.list {
		out = append(out, c.EffectiveContract(g))
	}
	return out
}

// EffectiveContract returns one named collection's effective contract.
// It resolves the name through Get, so asking for a collection the
// caller may not see fails with the identical ErrUnknownCollection
// message a name nobody ever mounted produces.
func (r *Registry) EffectiveContract(name string, g *authz.Grants) (EffectiveContract, error) {
	c, err := r.Get(name)
	if err != nil {
		return EffectiveContract{}, err
	}
	return c.EffectiveContract(g), nil
}
