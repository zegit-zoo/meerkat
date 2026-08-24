// Package mcp wires the embedded knowledge base to a Model Context
// Protocol server. The server exposes:
//
//	mk_search           - full-text search across all wiki pages
//	mk_show             - retrieve a single wiki page by ID
//	mk_list             - enumerate wiki pages with optional filters
//	mk_list_collections - enumerate mounted collections: name, backend
//	                      source type, page count and the CALLER's own
//	                      capabilities over each
//	mk_save_memory      - save a personal/team/global memory document
//	                      (only when a collection declares a memory: store)
//
// Every tool takes an optional "collection" argument. When several
// collections are mounted (see internal/collections), each tool's
// description names them, so a client discovers the set from the tool
// list it already fetches — there is no extra tool to call. Omitting
// the argument queries every collection; results carry the collection
// they came from, and a page ID can be qualified as
// "<collection>:<page-id>".
//
// Two transports serve the identical tool set:
//
//   - stdio (ServeStdio, `mk mcp serve`) — unauthenticated, spawned by
//     a local MCP client, running until its stdin closes.
//   - Streamable HTTP (ServeStreamableHTTP, `mk mcp serve-http` — see
//     hosted.go) — a hosted, concurrent, OIDC-authenticated server.
//
// Under the hosted transport every handler operates on the caller's
// own registry view rather than the mounted one: see visible (reads)
// and writable (memory writes), and docs/design/hosted-mcp.md for why
// an unauthorized collection has to be invisible rather than denied.
//
// That view narrows twice, in one expression: which COLLECTIONS the
// caller may read, and which PAGES they may see inside them. The second
// exists for personal memories, which are private to the principal who
// saved them (docs/design/memory.md). Under stdio no collection is ever
// filtered out, and the page viewer is the local user — who owns the one
// personal namespace a stdio session writes into, so a single-user
// deployment sees exactly what it always saw.
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
	"github.com/zegit-zoo/meerkat/internal/search"
)

// serverName and serverVersion identify meerkat in the MCP initialize
// handshake. Shared by both transports so a client sees the same server
// whichever way it connects.
const (
	serverName    = "meerkat"
	serverVersion = "0.2.0"
)

// visible returns the caller's read view: the collections ctx's caller
// may read, and within them the pages ctx's caller may see.
//
// This is the ONLY place the MCP surface consults authorization, and it
// does so by narrowing the registry rather than by checking a
// permission at each operation. Everything downstream — the handlers
// below, the tool descriptions, the ambiguity error, the "available:"
// list in an unknown-collection error, the page counts — then works from
// a registry that simply does not contain the collections this caller
// may not read, and whose pages are already the ones they may see.
//
// The two narrowings are deliberately both applied here, in one
// expression, rather than one here and one at each read:
//
//	Restrict(CanRead)  which COLLECTIONS exist for this caller
//	ViewedBy(viewer)   which PAGES exist inside them
//
// A context with no grants means no collection policy is in force
// (stdio, or a hosted deployment with no auth: block), so nothing is
// filtered out of the registry. The page viewer is applied regardless:
// it does not come from the policy, it comes from who the caller IS, and
// on stdio that is the local user (see transportOptions.viewer).
func visible(ctx context.Context, reg *collections.Registry, mem transportOptions) *collections.Registry {
	g := authz.FromContext(ctx)
	view := reg
	if g != nil {
		view = reg.Restrict(g.CanRead)
	}
	return view.ViewedBy(mem.viewer(g))
}

// newServer builds the MCP server and registers the tool set. Shared by
// both transports so they cannot drift: adding a tool here adds it to
// stdio and to the hosted HTTP server at once.
//
// mem carries the few decisions the memory tool cannot make for itself
// — see transportOptions — and is the one place the two transports
// deliberately differ.
//
// opts are appended after the defaults, so a transport can add its own
// server options (the hosted one adds a per-request tool filter and
// session hooks).
func newServer(reg *collections.Registry, mem transportOptions, opts ...mcpserver.ServerOption) *mcpserver.MCPServer {
	opts = append([]mcpserver.ServerOption{mcpserver.WithToolCapabilities(true)}, opts...)
	s := mcpserver.NewMCPServer(serverName, serverVersion, opts...)
	registerSearch(s, reg, mem)
	registerShow(s, reg, mem)
	registerList(s, reg, mem)
	registerListCollections(s, reg, mem)
	registerSaveMemory(s, reg, mem)
	return s
}

// indexAll builds every mounted collection's index.
//
// Lazy building is right for a CLI invocation, but a server should pay
// its startup cost at startup — not inside whichever request happens to
// be first — and should fail to start at all if a collection can't be
// indexed, rather than surfacing that as a tool error much later.
func indexAll(reg *collections.Registry) error {
	for _, c := range reg.All() {
		if _, err := c.Index(); err != nil {
			return fmt.Errorf("build search index for collection %q: %w", c.Name, err)
		}
	}
	return nil
}

// ServeStdio constructs an MCP server serving reg's collections and
// serves it on stdio until ctx is cancelled or stdin closes.
//
// Each collection's search index is built on first use and reused
// across calls. Bleve in-memory indexes are safe for concurrent reads.
//
// stdio is unauthenticated by construction: the transport is a pipe
// from a process the user already started, so there is no caller to
// identify and no collection to filter. No grants ever enter the context
// here, and every collection is visible.
//
// Per-page visibility still applies, and is a no-op for the deployment
// this transport exists for: the local user reads the `local` personal
// namespace, which is the only one a stdio session ever writes into. It
// bites only on a store SHARED with a hosted server, where the other
// namespaces belong to other people and were never this user's to read.
func ServeStdio(ctx context.Context, reg *collections.Registry) error {
	if err := indexAll(reg); err != nil {
		return err
	}
	defer func() { _ = reg.Close() }()

	// AllowAnonymousPersonal is true here and nowhere else: a stdio
	// server was spawned by the one user it serves, so a personal memory
	// has an unambiguous owner even though no token established it. See
	// transportOptions.
	return mcpserver.ServeStdio(newServer(reg, transportOptions{AllowAnonymousPersonal: true}))
}

// Tool names. Constants because the per-request tool filter
// (toolFilter) has to recognise a tool by name in order to rebuild its
// description for the caller, and a name that drifts between the
// registration and the filter would silently stop the rebuild — leaving
// the unfiltered, collection-naming description in place.
const (
	toolSearch          = "mk_search"
	toolShow            = "mk_show"
	toolList            = "mk_list"
	toolListCollections = "mk_list_collections"
	// toolSaveMemory is declared in memory.go, alongside the tool it
	// names.
)

// toolFilter rebuilds each tool's definition against the caller's own
// registry views, and removes a tool entirely from a caller who has no
// collection it could act on.
//
// It exists because a tool DESCRIPTION is an enumeration surface: the
// mounted collection names are written into mk_search/mk_show/mk_list's
// prose and into the "collection" argument's description, precisely so
// a client discovers them from the tool list it already fetches. Left
// alone under authorization, tools/list would hand every caller the
// full mounted set — the same leak as an unfiltered GET /collections,
// arriving through the one surface every MCP client reads first.
// mk_save_memory names collections in exactly the same way and is
// filtered by exactly the same mechanism.
//
// mcp-go applies tool filters to tools/call as well as tools/list, so
// this is an access boundary and not only a display fix: a caller with
// no readable collections cannot invoke the read tools either, and one
// with nowhere to write cannot invoke the memory tool.
//
// The two views are kept apart on purpose. A caller granted only
// `personal-write` on one collection reads nothing and gets no KB
// tools, but must still be offered mk_save_memory; a caller granted
// only `read` gets the KB tools and no memory tool. Collapsing them
// into one view would silently take one of those two away.
func toolFilter(reg *collections.Registry) mcpserver.ToolFilterFunc {
	return func(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
		g := authz.FromContext(ctx)
		if g == nil {
			return tools
		}
		readView := reg.Restrict(g.CanRead)
		memView := writable(ctx, reg)
		out := make([]mcp.Tool, 0, len(tools))
		for _, t := range tools {
			switch t.Name {
			case toolSearch:
				if readView.Len() > 0 {
					out = append(out, searchTool(readView))
				}
			case toolShow:
				if readView.Len() > 0 {
					out = append(out, showTool(readView))
				}
			case toolList:
				if readView.Len() > 0 {
					out = append(out, listTool(readView))
				}
			case toolListCollections:
				if readView.Len() > 0 {
					out = append(out, listCollectionsTool(readView))
				}
			case toolSaveMemory:
				if memView.Len() > 0 {
					out = append(out, saveMemoryTool(memView))
				}
			default:
				out = append(out, t)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
}

// collectionArg builds the shared, optional "collection" argument. Its
// description carries the mounted collection names, which is how a
// client discovers what it may pass — the tool list every MCP client
// already fetches doubles as the collection listing.
func collectionArg(reg *collections.Registry) mcp.ToolOption {
	desc := "Optional. Restrict to one collection. "
	if reg.Single() {
		desc += "This server mounts a single collection (" + reg.Names()[0] + "), so this can be omitted."
	} else {
		desc += "Mounted collections: " + strings.Join(reg.Names(), ", ") +
			". Omit to query all of them; every result names the collection it came from."
	}
	return mcp.WithString("collection", mcp.Description(desc))
}

// collectionSuffix is the sentence appended to every tool description
// so the multi-collection contract is visible without reading the
// argument schema.
func collectionSuffix(reg *collections.Registry) string {
	if reg.Single() {
		return ""
	}
	return " This server mounts " + fmt.Sprintf("%d", reg.Len()) + " collections (" +
		strings.Join(reg.Names(), ", ") + "); results carry a 'collection' field, and a page ID " +
		"may be written as '<collection>:<page-id>'."
}

func registerSearch(s *mcpserver.MCPServer, reg *collections.Registry, mem transportOptions) {
	s.AddTool(searchTool(reg), searchHandler(reg, mem))
}

// searchTool builds the mk_search tool definition. Split out from
// registerSearch so its annotations (see the inline comment below) are
// directly unit-testable without standing up a server or driving stdio.
func searchTool(reg *collections.Registry) mcp.Tool {
	return mcp.NewTool(toolSearch,
		mcp.WithDescription(
			"Full-text search across the knowledge base "+
				"(meerkat). Title and ID matches are boosted so "+
				"page-name lookups (e.g. a concept or service name) "+
				"rank highest. Returns a JSON list of "+
				"{id, collection, title, score, snippet, category} hits."+
				collectionSuffix(reg)),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Free-text query. Supports phrases ('like this'), field targeting (title:foo), and boolean operators."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results (default 10)."),
		),
		collectionArg(reg),
		// mcp.NewTool defaults every tool's annotations to the MCP spec's
		// safe-but-wrong-for-us shape (readOnlyHint:false,
		// destructiveHint:true, idempotentHint:false, openWorldHint:true)
		// unless overridden here. mk_search only ever reads the in-memory
		// bleve index built at startup: it can't modify anything, repeated
		// calls with the same query are indistinguishable from one call,
		// and it never reaches outside this process. Leaving the defaults
		// in place is what a hardening scanner flags, and it makes
		// harnesses that gate on destructiveHint refuse a plain KB lookup.
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)
}

// searchHandler returns the mk_search tool handler bound to reg. Split out
// (and the formatting moved to searchResultsJSON) so the query→result-shape
// path is unit-testable against an injected registry.
func searchHandler(reg *collections.Registry, mem transportOptions) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		limit := 0
		if v := req.GetFloat("limit", 0); v > 0 {
			limit = int(v)
		}

		// Bound this query's execution server-side (see
		// search.DefaultQueryTimeout) even if the MCP client is happy to
		// wait forever; ctx itself already carries any cancellation the
		// client/transport imposes. Either reaches bleve's collector via
		// Index.QueryContext -> bleve.SearchInContext.
		ctx, cancel := context.WithTimeout(ctx, search.DefaultQueryTimeout)
		defer cancel()

		results, err := visible(ctx, reg, mem).Search(ctx, req.GetString("collection", ""), query, limit)
		if err != nil {
			switch {
			case errors.Is(err, search.ErrInvalidQuery), errors.Is(err, collections.ErrUnknownCollection):
				// A rejected input (oversized / pathologically nested
				// query, or a collection that isn't mounted), not an
				// internal failure — surface it as a normal tool error,
				// not a transport-level error.
				return mcp.NewToolResultError(err.Error()), nil
			case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
				return mcp.NewToolResultError("search: query did not complete within the maximum query duration"), nil
			default:
				return nil, fmt.Errorf("search: %w", err)
			}
		}
		body, err := searchResultsJSON(results)
		if err != nil {
			return nil, fmt.Errorf("encode results: %w", err)
		}
		return mcp.NewToolResultText(body), nil
	}
}

// searchResultsJSON renders search results into the stable mk_search wire
// shape: a JSON array of {id, collection, title, category, status, score,
// snippet}. id stays the page's own unqualified ID — the collection is a
// field of its own, never folded into it.
func searchResultsJSON(results []collections.Hit) (string, error) {
	simplified := make([]map[string]any, len(results))
	for i, r := range results {
		simplified[i] = map[string]any{
			"id":         r.Page.ID,
			"collection": r.Collection,
			"title":      r.Page.Title,
			"category":   r.Page.Front.Category,
			"status":     r.Page.Front.Status,
			"score":      r.Score,
			"snippet":    oneLine(r.Snippet),
		}
	}
	body, err := json.MarshalIndent(simplified, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func registerShow(s *mcpserver.MCPServer, reg *collections.Registry, mem transportOptions) {
	s.AddTool(showTool(reg), showHandler(reg, mem))
}

// showTool builds the mk_show tool definition. Split out from
// registerShow for the same reason as searchTool: direct annotation
// unit-testing.
func showTool(reg *collections.Registry) mcp.Tool {
	return mcp.NewTool(toolShow,
		mcp.WithDescription(
			"Retrieve a single knowledge-base page by ID. Page IDs are "+
				"slash-separated paths from the wiki root without "+
				"the .md suffix, e.g. 'concepts/Some-Concept', "+
				"'systems/backend/some-service'. Returns a JSON "+
				"object {id, collection, title, body, front, trust_tier, stale} "+
				"where front is the parsed frontmatter (type, "+
				"description, category, owner, status, source, "+
				"related, tags, generated, verified, stale_after, ...) "+
				"and trust_tier/stale are advisory signals OKF derives "+
				"from front.verified/front.stale_after: trust_tier is "+
				"one of unverified | machine-confirmed | human-reviewed; "+
				"stale is true once today is on/after stale_after."+
				collectionSuffix(reg)),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Page ID, e.g. 'concepts/Some-Concept'. May be qualified as '<collection>:<page-id>'."),
		),
		collectionArg(reg),
		// See registerSearch's comment: mk_show only reads a page out of
		// the KB filesystem, so it gets the same read-only/non-destructive/
		// idempotent/closed-world annotations instead of NewTool's defaults.
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)
}

// showHandler returns the mk_show tool handler. Split out to mirror
// searchHandler's shape.
func showHandler(reg *collections.Registry, mem transportOptions) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ref, err := visible(ctx, reg, mem).Show(req.GetString("collection", ""), id)
		if err != nil {
			switch {
			case errors.Is(err, kb.ErrNotFound):
				return mcp.NewToolResultError(
					fmt.Sprintf("page %q not found - try the mk_list tool", id),
				), nil
			case errors.Is(err, collections.ErrAmbiguous), errors.Is(err, collections.ErrUnknownCollection):
				// Both are caller-fixable: the error text already names
				// the qualified IDs (or the mounted collections) to
				// choose from, so hand it back as a tool error the model
				// can act on rather than a transport failure.
				return mcp.NewToolResultError(err.Error()), nil
			}
			return nil, err
		}
		body, err := showPageJSON(ref)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(body), nil
	}
}

// showPageOutput is the mk_show wire shape: the page's stored fields
// (embedded, so id/title/body/front are promoted to the top level
// exactly as before this change) plus the collection it was served from
// and two OKF-derived advisory signals that are computed rather than
// stored — see kb.Frontmatter.TrustTier / IsStale — and so don't
// already appear on kb.Page's own JSON encoding.
type showPageOutput struct {
	kb.Page
	Collection string `json:"collection"`
	TrustTier  string `json:"trust_tier"`
	Stale      bool   `json:"stale"`
}

// showPageJSON renders showPageOutput for a page reference. Split out
// from showHandler so the shape is directly unit-testable, mirroring
// searchResultsJSON / listPagesJSON.
func showPageJSON(ref collections.PageRef) (string, error) {
	out := showPageOutput{
		Page:       ref.Page,
		Collection: ref.Collection,
		TrustTier:  ref.Page.Front.TrustTier(),
		Stale:      ref.Page.Front.IsStale(time.Now().UTC()),
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func registerList(s *mcpserver.MCPServer, reg *collections.Registry, mem transportOptions) {
	s.AddTool(listTool(reg), listHandler(reg, mem))
}

// listTool builds the mk_list tool definition. Split out from
// registerList for the same reason as searchTool: direct annotation
// unit-testing.
func listTool(reg *collections.Registry) mcp.Tool {
	return mcp.NewTool(toolList,
		mcp.WithDescription(
			"List knowledge-base pages, optionally filtered. Filters "+
				"compose (AND): prefix (page ID prefix), category "+
				"(frontmatter category, e.g. 'policies', 'systems', "+
				"'adr', 'concepts'), status (e.g. 'placeholder', "+
				"'reviewed'), owner, type (frontmatter 'type', OKF's "+
				"concept-kind field, e.g. 'BigQuery Table', 'Metric'). "+
				"Returns [{id, collection, title, category, status, owner, type, source}]."+
				collectionSuffix(reg)),
		mcp.WithString("prefix",
			mcp.Description("ID prefix filter, e.g. 'systems/backend/'."),
		),
		mcp.WithString("category",
			mcp.Description("Frontmatter category filter, e.g. 'policies'."),
		),
		mcp.WithString("status",
			mcp.Description("Frontmatter status filter, e.g. 'placeholder', 'reviewed', 'stale'."),
		),
		mcp.WithString("owner",
			mcp.Description("Frontmatter owner filter."),
		),
		mcp.WithString("type",
			mcp.Description("Frontmatter type filter (OKF's concept-kind field), e.g. 'BigQuery Table', 'Metric', 'Playbook'."),
		),
		collectionArg(reg),
		// See registerSearch's comment: mk_list only enumerates pages
		// already loaded from the KB filesystem, so it gets the same
		// read-only/non-destructive/idempotent/closed-world annotations
		// instead of NewTool's defaults.
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)
}

// listHandler returns the mk_list tool handler. Split out to mirror
// searchHandler's shape.
func listHandler(reg *collections.Registry, mem transportOptions) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		refs, err := visible(ctx, reg, mem).Pages(req.GetString("collection", ""))
		if err != nil {
			if errors.Is(err, collections.ErrUnknownCollection) {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return nil, err
		}
		refs = filterPages(refs,
			req.GetString("prefix", ""),
			req.GetString("category", ""),
			req.GetString("status", ""),
			req.GetString("owner", ""),
			req.GetString("type", ""),
		)
		body, err := listPagesJSON(refs)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(body), nil
	}
}

// filterPages applies the composable (AND) mk_list filters. Empty filters
// are no-ops. Split out so the filter composition is unit-testable without
// the embedded KB.
func filterPages(refs []collections.PageRef, prefix, category, status, owner, typ string) []collections.PageRef {
	for _, f := range []kb.FilterFunc{
		kb.ByPrefix(prefix),
		filterIf(category != "", kb.ByCategory(category)),
		filterIf(status != "", kb.ByStatus(status)),
		filterIf(owner != "", kb.ByOwner(owner)),
		filterIf(typ != "", kb.ByType(typ)),
	} {
		if f == nil {
			continue
		}
		out := refs[:0:0]
		for _, r := range refs {
			if f(r.Page) {
				out = append(out, r)
			}
		}
		refs = out
	}
	return refs
}

// filterIf returns f only when cond holds, so an unset filter is a nil
// FilterFunc — the same "nil keeps everything" convention kb.ByPrefix
// already uses for an empty prefix.
func filterIf(cond bool, f kb.FilterFunc) kb.FilterFunc {
	if !cond {
		return nil
	}
	return f
}

// listPagesJSON renders the mk_list wire shape: a JSON array of
// {id, collection, title, category, status, owner, type, source}.
func listPagesJSON(refs []collections.PageRef) (string, error) {
	out := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		p := r.Page
		out = append(out, map[string]any{
			"id":         p.ID,
			"collection": r.Collection,
			"title":      p.Title,
			"category":   p.Front.Category,
			"status":     p.Front.Status,
			"owner":      p.Front.Owner,
			"type":       p.Front.Type,
			"source":     p.Front.Source,
		})
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func registerListCollections(s *mcpserver.MCPServer, reg *collections.Registry, mem transportOptions) {
	s.AddTool(listCollectionsTool(reg), listCollectionsHandler(reg, mem))
}

// listCollectionsTool builds the mk_list_collections tool definition.
// Split out from registerListCollections for the same reason as
// searchTool: direct annotation unit-testing, and so toolFilter can
// rebuild it against the caller's own view (its description names the
// mounted collections exactly as collectionArg's does, so it is an
// enumeration surface too and gets the same per-request treatment).
func listCollectionsTool(reg *collections.Registry) mcp.Tool {
	desc := "Enumerate the knowledge-base collections available on this server: " +
		"what each is named, its backend source type, how many pages it holds, " +
		"and the capabilities YOU hold on it (e.g. [\"read\"], [\"read\",\"personal-write\"]). " +
		"Takes no arguments. Call this before naming an unfamiliar 'collection' value on " +
		"mk_search/mk_show/mk_list, or to check whether mk_save_memory is worth trying. " +
		"Returns a JSON array of {name, type, source, pages, capabilities, description?, contract?}. " +
		"When present, 'contract' is the sanctioned way to contribute knowledge back to " +
		"that collection, already adjusted to YOUR capabilities: method 'direct' means " +
		"write through this server; 'merge-request' means patch the contribution repo it " +
		"names (repo/host/branch/path/instructions tell you how); 'staging' means submit " +
		"via mk_save_memory for review; 'none' plus a reason means you currently have no " +
		"contribution path. " +
		"A hosted server lists only the collections you may read; one you cannot read is " +
		"absent from the result, not merely empty."
	if reg.Single() {
		desc += " This server currently mounts a single collection (" + reg.Names()[0] + ")."
	} else {
		desc += " Mounted collections: " + strings.Join(reg.Names(), ", ") + "."
	}
	return mcp.NewTool(toolListCollections,
		mcp.WithDescription(desc),
		// See registerSearch's comment: this tool only reads registry and
		// collection metadata already resolved at startup, so it gets the
		// same read-only/non-destructive/idempotent/closed-world
		// annotations instead of NewTool's defaults.
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)
}

// listCollectionsHandler returns the mk_list_collections handler bound
// to reg. It takes no arguments — see visible() for the hosted-mode
// filtering, applied here exactly as it is for mk_search/mk_show/
// mk_list.
func listCollectionsHandler(reg *collections.Registry, mem transportOptions) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		body, err := listCollectionsJSON(ctx, visible(ctx, reg, mem))
		if err != nil {
			return nil, fmt.Errorf("encode collections: %w", err)
		}
		return mcp.NewToolResultText(body), nil
	}
}

// collectionSummary is the mk_list_collections wire shape for one
// mounted collection: the same name/type/source/pages that `mk list
// --collections` (internal/cli/list.go) and GET /collections
// (internal/http/server.go) report, so all three surfaces agree on what
// those fields mean, plus the CALLER's own effective capabilities over
// it — something only an authz-aware transport can compute, so it lives
// here and not in the other two.
//
// Description and Contract are omitempty additions (issue #23): an old
// client parsing the original shape is unaffected, a new one gets them
// for free. Contract is present only when the operator declared an
// update: block for the collection — `method: none` (or no block) is a
// statement that there is nothing to say, per the update-contract
// design doc.
type collectionSummary struct {
	Name         string           `json:"name"`
	Type         string           `json:"type"`
	Source       string           `json:"source"`
	Pages        int              `json:"pages"`
	Capabilities []string         `json:"capabilities"`
	Description  string           `json:"description,omitempty"`
	Contract     *contractSummary `json:"contract,omitempty"`
	// Refresh reports runtime reconciliation for this collection, one
	// entry per configured target, and is absent for a collection that
	// is resolved once at startup — which is every collection that has
	// not opted in.
	//
	// This is where the detailed freshness provenance lives: which
	// generation is applied, when the last cycle succeeded, and whether
	// the collection is currently degraded. It is HERE rather than in
	// /readyz or a metric label because this surface is authenticated
	// and already scoped to the collections this caller may read — the
	// unauthenticated endpoints get counts, and a metric labelled with a
	// generation would mint a series per publication. See
	// docs/design/hot-reload.md.
	Refresh []collections.ReloadStatus `json:"refresh,omitempty"`
}

// contractSummary is the wire shape of the caller-effective
// contribution contract inside a collectionSummary. It is
// collections.EffectiveContract minus the fields collectionSummary
// already carries (collection name, description): Method is what THIS
// caller should do, Declared what the operator configured — when they
// differ, Reason says why (e.g. a missing publish capability demoted
// direct to staging).
type contractSummary struct {
	Method       string `json:"method"`
	Declared     string `json:"declared_method"`
	Repo         string `json:"repo,omitempty"`
	Host         string `json:"host,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Path         string `json:"path,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// listCollectionsJSON renders the mk_list_collections wire shape: a
// JSON array of collectionSummary, one per collection in view, in
// configuration order.
//
// view is already the caller's restricted read view (see visible): a
// collection this caller may not read was filtered out before this
// function ever sees it, so there is nothing left to check here — the
// same invisibility guarantee mk_search/mk_show/mk_list get from
// routing through visible().
//
// Capabilities comes from authz.Grants.Capabilities, which is nil-safe:
// under stdio, or a hosted server with no auth: configured, ctx carries
// no grants and Capabilities reports the full capability set — there is
// no identity to restrict against, the same reasoning mk_save_memory's
// AllowAnonymousPersonal applies to anonymous/local mode (see
// memory.go).
func listCollectionsJSON(ctx context.Context, view *collections.Registry) (string, error) {
	g := authz.FromContext(ctx)
	out := make([]collectionSummary, 0, view.Len())
	for _, c := range view.All() {
		entry := collectionSummary{
			Name:         c.Name,
			Type:         c.Type(),
			Source:       c.Provenance(),
			Capabilities: g.Capabilities(c.Name).Strings(),
			Description:  c.Description(),
			Refresh:      c.ReloadStatuses(),
		}
		// view.Pages, not c.Pages: a page count is a count, and a count
		// that included other principals' personal memories would say how
		// many of them there are. Routing through the view applies the
		// same viewer mk_list does, so the number a caller is told matches
		// the number they can enumerate.
		if pages, err := view.Pages(c.Name); err == nil {
			entry.Pages = len(pages)
		}
		if c.Contract().Declared() {
			ec := c.EffectiveContract(g)
			entry.Contract = &contractSummary{
				Method:       string(ec.Method),
				Declared:     string(ec.Declared),
				Repo:         ec.Repo,
				Host:         ec.Host,
				Branch:       ec.Branch,
				Path:         ec.Path,
				Instructions: ec.Instructions,
				Reason:       ec.Reason,
			}
		}
		out = append(out, entry)
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
