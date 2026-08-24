package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// memory.go is the writable half of the MCP surface: mk_save_memory.
//
// Everything security-relevant about it fits in four sentences.
//
//  1. The collection is resolved through the caller's WRITABLE registry
//     view (see writable), so a collection they hold nothing over is as
//     invisible here as it is to mk_search — the unknown-collection
//     error names only what they can already see.
//  2. The scope's capability is then checked on top of that view, per
//     call (memory.Authorize). Restrict is read-shaped; a write needs
//     more than visibility.
//  3. The namespace a personal memory lands in comes from
//     authz.Grants.Identity() and from nowhere else. No tool argument
//     reaches it. A crafted key, scope or collection changes where in
//     the caller's own space a memory goes, never whose space it is.
//  4. Every write carries an optimistic-locking precondition, so two
//     agents saving the same key cannot silently lose one of the two
//     memories.
//
// Design: docs/design/memory.md.

// toolSaveMemory is the tool name. A constant for the same reason the
// read tools' names are: the per-request filter recognises a tool by
// name in order to rebuild its description, and a name that drifted
// would silently leave the unfiltered description in place.
const toolSaveMemory = "mk_save_memory"

// transportOptions are the transport-level decisions the memory toolset
// needs and cannot work out for itself. There is exactly one, and it
// answers the same question on both sides of the memory feature: when
// nobody authenticated, WHO is this?
type transportOptions struct {
	// AllowAnonymousPersonal says the anonymous caller is the single
	// local user, and may therefore both write and read the fixed
	// `local` personal namespace.
	//
	// True on stdio ONLY. A stdio server was spawned by the single user
	// it serves, over a pipe, with no identity to establish and nobody to
	// be confused with — refusing personal memories there would make the
	// feature useless in exactly the setting it is most useful. On the
	// hosted transport it is false, including when
	// allow_unauthenticated delegates authentication to a gateway: in
	// that deployment meerkat genuinely does not know who is calling, and
	// every caller would otherwise share one namespace.
	AllowAnonymousPersonal bool
}

// viewer derives the per-page read viewer for the caller behind g. It is
// the read-side counterpart of memory.Authorize, and it obeys the same
// rule: the owner comes out of the VERIFIED identity, never out of a
// request.
//
//	verified identity          -> that principal's namespace; they see
//	                              their own personal memories and no
//	                              other principal's
//	anonymous, stdio           -> the fixed `local` namespace, which is
//	                              the one this user's own memories are
//	                              written into — single-user behaviour
//	                              unchanged
//	anonymous, hosted          -> no owner at all: every public page,
//	                              and no personal memory belonging to
//	                              anyone
//
// The last row is the one worth arguing. A hosted server with no
// providers (or with allow_unauthenticated) cannot tell its callers
// apart, so it cannot tell which of them a personal memory belongs to
// — and it already refuses to let them WRITE one for exactly that
// reason. Handing every such caller the `local` namespace would give
// them, as a group, read access to whatever a stdio session wrote into a
// shared store. Owning nothing is the only answer that stays true when
// meerkat does not know who is asking.
func (o transportOptions) viewer(g *authz.Grants) kb.Viewer {
	id := g.Identity()
	if memory.Anonymous(id) && !o.AllowAnonymousPersonal {
		return kb.AsOwner("")
	}
	return kb.AsOwner(memory.Namespace(id))
}

// writable returns the collections ctx's caller may write memories to:
// the ones they hold a write capability over, narrowed to the ones that
// HAVE a memory store.
//
// It is the write path's exact analogue of visible(), and it is a
// separate view rather than a reuse of visible() because
// Restrict(CanRead) is read-shaped in both directions:
//
//   - a rule granting `personal-write` without `read` is unusual but
//     coherent ("drop notes here, don't read the others'"), and its
//     holder must be able to name the collection they were granted —
//     which a read-shaped view would hide from them;
//   - a rule granting only `read` makes the collection visible, and its
//     holder must NOT be offered a tool they can only ever be refused
//     by — which a read-shaped view would offer.
//
// Narrowing to CanWrite is strictly tighter than the read view, so it
// can leak nothing the read path does not already: a collection the
// caller holds nothing over is filtered out here exactly as it is by
// visible(), and every error path below — the unknown-collection
// sentence, the collection list in the tool description — names only
// collections the caller was explicitly granted a write on.
func writable(ctx context.Context, reg *collections.Registry) *collections.Registry {
	g := authz.FromContext(ctx)
	if g == nil {
		return reg.WithMemory()
	}
	return reg.Restrict(g.CanWrite).WithMemory()
}

// registerSaveMemory adds the memory tool, but only when at least one
// mounted collection has a memory store. A deployment that configured
// none sees no new tool at all — the same back-compat rule the rest of
// meerkat follows: a feature nobody enabled changes nothing.
func registerSaveMemory(s *mcpserver.MCPServer, reg *collections.Registry, opts transportOptions) {
	if len(reg.MemoryNames()) == 0 {
		return
	}
	s.AddTool(saveMemoryTool(reg.WithMemory()), saveMemoryHandler(reg, opts))
}

// saveMemoryTool builds the mk_save_memory definition against the
// collections in view. Split out from registerSaveMemory so the
// per-request filter can rebuild it for each caller.
func saveMemoryTool(view *collections.Registry) mcp.Tool {
	names := view.Names()
	target := "Collections accepting memories: " + strings.Join(names, ", ") +
		". Name one with the 'collection' argument."
	if len(names) == 1 {
		target = "Writes into the one collection that accepts memories (" + names[0] + "), so 'collection' can be omitted."
	}
	return mcp.NewTool(toolSaveMemory,
		mcp.WithDescription(
			"Save a durable memory to the knowledge base (meerkat), as a "+
				"Markdown document that is searchable by mk_search and "+
				"retrievable by mk_show the moment this call returns. Use it "+
				"for facts worth keeping across sessions — a decision and its "+
				"reasoning, a convention, a correction the user made. "+
				"Scopes: 'personal' (yours alone; the namespace comes from "+
				"your authenticated identity and cannot be chosen), 'team' "+
				"(shared with the collection's writers), 'global' (every "+
				"reader of the collection). A team or global memory you are "+
				"not authorized to write is saved as a pending review "+
				"artifact instead, and the response says so. "+target),
		mcp.WithString("scope",
			mcp.Required(),
			mcp.Description("One of: "+strings.Join(memory.ScopeNames(), ", ")+
				". 'personal' is the safe default for anything you are not sure everyone should see."),
			mcp.Enum(memory.ScopeNames()...),
		),
		mcp.WithString("title",
			mcp.Required(),
			mcp.Description("A short human title for the memory. Also the highest-boosted search field."),
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("The memory itself, as Markdown. Stored verbatim as the document body."),
		),
		mcp.WithString("key",
			mcp.Description("Stable identifier for this memory, so a later save can update it rather than "+
				"creating a second copy. Slugified (lowercased; anything that is not a letter, digit or "+
				"underscore becomes '-'), and always a single name inside your own scope — it cannot "+
				"select a namespace, a collection or another caller's space. Defaults to a slug of the title."),
		),
		mcp.WithArray("tags",
			mcp.Description("Optional frontmatter tags."),
			mcp.WithStringItems(),
		),
		mcp.WithString("version",
			mcp.Description("The 'version' a previous save of this exact memory returned. Supplying it makes "+
				"this an update conditioned on that revision: if anything else changed the memory in the "+
				"meantime the call fails with a conflict instead of overwriting their work. Omit for a new memory."),
		),
		mcp.WithBoolean("replace",
			mcp.Description("Update an existing memory at this key without knowing its version (meerkat reads "+
				"the current one and conditions the write on it). Without this, and without 'version', saving "+
				"over an existing memory is a conflict. Ignored when 'version' is given."),
		),
		mcp.WithString("collection",
			mcp.Description("Optional. The collection to save into. "+target),
		),
		// mcp.NewTool's annotation defaults are wrong in the opposite
		// direction here from the read tools: this one DOES modify state,
		// so readOnlyHint stays false. It is not destructive (it adds or
		// updates one document and removes nothing), and it is not
		// idempotent — a second call with no key writes a second document,
		// and a second call with the same key and a stale version is a
		// conflict, which is the whole point. openWorldHint is true for a
		// GCS-backed store, which is a real external system.
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

// saveMemoryArgs is the parsed, validated request.
type saveMemoryArgs struct {
	collection string
	scope      memory.Scope
	doc        memory.Document
	version    memory.Version
	replace    bool
}

// parseSaveMemoryArgs reads and validates the tool arguments. Note what
// it does NOT read: there is no subject, namespace, owner or author
// argument to parse, because there is none to offer.
func parseSaveMemoryArgs(req mcp.CallToolRequest) (saveMemoryArgs, error) {
	var a saveMemoryArgs
	rawScope, err := req.RequireString("scope")
	if err != nil {
		return a, err
	}
	scope, ok := memory.ParseScope(rawScope)
	if !ok {
		return a, fmt.Errorf("unknown scope %q — one of %s", rawScope, strings.Join(memory.ScopeNames(), ", "))
	}
	title, err := req.RequireString("title")
	if err != nil {
		return a, err
	}
	content, err := req.RequireString("content")
	if err != nil {
		return a, err
	}
	if strings.TrimSpace(content) == "" {
		return a, errors.New("content is empty — there is nothing to remember")
	}
	a.scope = scope
	a.collection = req.GetString("collection", "")
	a.version = memory.Version(strings.TrimSpace(req.GetString("version", "")))
	a.replace = req.GetBool("replace", false)
	a.doc = memory.Document{
		Key:   req.GetString("key", ""),
		Title: title,
		Body:  content,
		Tags:  req.GetStringSlice("tags", nil),
	}
	return a, nil
}

// saveMemoryHandler returns the mk_save_memory handler bound to reg.
func saveMemoryHandler(reg *collections.Registry, opts transportOptions) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseSaveMemoryArgs(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Resolve through the caller's writable view. Everything about
		// invisibility that #9 established for reads holds here for the
		// same reason: the error below names only this registry's
		// collections, which are only ever the caller's own.
		view := writable(ctx, reg)
		col, err := targetMemoryCollection(view, args.collection)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		decision, err := memory.Authorize(authz.FromContext(ctx), col.Name, args.scope, opts.AllowAnonymousPersonal)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		ref, err := memory.Resolve(args.doc, args.scope, decision.Namespace)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		if decision.Outcome == memory.OutcomeStage {
			return stageMemory(ctx, col, args, ref, decision)
		}
		return saveMemory(ctx, col, args, ref, decision)
	}
}

// targetMemoryCollection picks the collection to write to: the named
// one, or the only one when exactly one is in view.
//
// An omitted collection with several in view is an ERROR naming them,
// not a pick. Guessing which knowledge base a memory belongs in is the
// one mistake here that cannot be undone by the caller — they would not
// know it had happened.
func targetMemoryCollection(view *collections.Registry, name string) (*collections.Collection, error) {
	if name != "" {
		return view.Get(name)
	}
	switch view.Len() {
	case 0:
		return nil, errors.New("no collection here accepts memories")
	case 1:
		return view.All()[0], nil
	default:
		return nil, fmt.Errorf("several collections accept memories (%s) — name one with the 'collection' argument",
			strings.Join(view.Names(), ", "))
	}
}

// saveMemory performs an authorized write.
func saveMemory(ctx context.Context, col *collections.Collection, args saveMemoryArgs, ref memory.Ref, d memory.Decision) (*mcp.CallToolResult, error) {
	// Resolve the store ONCE. The collection can be closed out from
	// under a request in flight (shutdown), and reaching for it twice
	// would mean the second reach could find nil.
	store := col.Memory()
	if store == nil {
		return mcp.NewToolResultError("this collection no longer accepts memories"), nil
	}
	body, err := memory.Render(args.doc, ref, d.Identity, memory.StatusLive, time.Now())
	if err != nil {
		return nil, err
	}
	pre, err := precondition(ctx, store, ref.Key, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	version, page, err := col.SaveMemory(ctx, ref.Key, body, pre)
	if err != nil {
		var conflict *memory.ConflictError
		if errors.As(err, &conflict) {
			return mcp.NewToolResultError(conflictMessage(ref, conflict)), nil
		}
		if errors.Is(err, memory.ErrConflict) {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return nil, fmt.Errorf("save memory: %w", err)
	}

	out := map[string]any{
		"status":     "saved",
		"collection": col.Name,
		"scope":      string(ref.Scope),
		"namespace":  ref.Namespace,
		"id":         page.ID,
		"version":    string(version),
		"location":   store.Location(ref.Key),
		"searchable": true,
		"note": "Saved and indexed. mk_search and mk_show can find it now. " +
			"Pass this 'version' back as the 'version' argument to update this same memory later.",
	}
	return jsonResult(out)
}

// stageMemory writes a pending review artifact for a caller who may
// write here, but not at this scope.
func stageMemory(ctx context.Context, col *collections.Collection, args saveMemoryArgs, ref memory.Ref, d memory.Decision) (*mcp.CallToolResult, error) {
	body, err := memory.Render(args.doc, ref, d.Identity, memory.StatusPending, time.Now())
	if err != nil {
		return nil, err
	}
	location, err := col.StageMemory(ctx, ref.StagingKey(), body)
	if err != nil {
		return nil, fmt.Errorf("stage memory: %w", err)
	}
	out := map[string]any{
		"status":     "staged",
		"collection": col.Name,
		"scope":      string(ref.Scope),
		"namespace":  ref.Namespace,
		"id":         ref.PageID,
		"location":   location,
		"searchable": false,
		"note": fmt.Sprintf("You are not authorized to write %s memories to %q, so this was saved as a "+
			"pending review artifact instead. It is NOT searchable and NOT visible to anyone until a "+
			"holder of the %q capability promotes it. Tell the user where it landed.",
			ref.Scope, col.Name, ref.Scope.Capability()),
	}
	return jsonResult(out)
}

// precondition builds the optimistic-locking condition for a write.
//
//	version given      -> update exactly that revision
//	replace: true      -> read the current revision and update from it
//	neither            -> create-only
//
// The replace path is still a compare-and-swap, not an unconditional
// overwrite: the version is read here and the write is conditioned on
// it, so a save that interleaves between the two fails rather than
// clobbering somebody's work. It is a convenience for the CALLER (who
// no longer has to carry a version around), never a relaxation of the
// guarantee.
func precondition(ctx context.Context, store memory.Store, key string, args saveMemoryArgs) (memory.Precondition, error) {
	if args.version != "" {
		return memory.UpdateFrom(args.version), nil
	}
	if !args.replace {
		return memory.CreateOnly(), nil
	}
	current, exists, err := store.Stat(ctx, key)
	if err != nil {
		return memory.Precondition{}, fmt.Errorf("read the current memory: %w", err)
	}
	if !exists {
		return memory.CreateOnly(), nil
	}
	return memory.UpdateFrom(current), nil
}

// conflictMessage renders an optimistic-locking failure as something a
// model can act on: what happened, and the exact next call to make.
func conflictMessage(ref memory.Ref, c *memory.ConflictError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "conflict: the memory %q was created or changed by someone else since you last read it, "+
		"so this save was refused rather than overwriting it.", ref.PageID)
	if c.Current != "" {
		fmt.Fprintf(&b, " It is now at version %q. Read it with mk_show, merge what you wanted to add, "+
			"and save again with version=%q.", c.Current, c.Current)
	} else {
		b.WriteString(" Read it with mk_show and save again with the version that returns.")
	}
	return b.String()
}

// jsonResult renders a tool result body.
func jsonResult(out map[string]any) (*mcp.CallToolResult, error) {
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return mcp.NewToolResultText(string(body)), nil
}
