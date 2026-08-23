// Package mcp wires the embedded knowledge base to a Model Context
// Protocol server. The server exposes:
//
//	mk_search - full-text search across all wiki pages
//	mk_show   - retrieve a single wiki page by ID
//	mk_list   - enumerate wiki pages with optional filters
//
// Every tool takes an optional "collection" argument. When several
// collections are mounted (see internal/collections), each tool's
// description names them, so a client discovers the set from the tool
// list it already fetches — there is no extra tool to call. Omitting
// the argument queries every collection; results carry the collection
// they came from, and a page ID can be qualified as
// "<collection>:<page-id>".
//
// The transport is stdio; the server runs until the parent process
// closes its stdin (or context cancellation).
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

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// ServeStdio constructs an MCP server serving reg's collections and
// serves it on stdio until ctx is cancelled or stdin closes.
//
// Each collection's search index is built on first use and reused
// across calls. Bleve in-memory indexes are safe for concurrent reads.
func ServeStdio(ctx context.Context, reg *collections.Registry) error {
	// Build every mounted collection's index up front. Lazy building is
	// right for a CLI invocation, but a server should pay its startup
	// cost at startup — not inside whichever request happens to be first
	// — and should fail to start at all if a collection can't be indexed,
	// rather than surfacing that as a tool error much later.
	for _, c := range reg.All() {
		if _, err := c.Index(); err != nil {
			return fmt.Errorf("build search index for collection %q: %w", c.Name, err)
		}
	}
	defer func() { _ = reg.Close() }()

	s := mcpserver.NewMCPServer(
		"meerkat",
		"0.2.0",
		mcpserver.WithToolCapabilities(true),
	)

	registerSearch(s, reg)
	registerShow(s, reg)
	registerList(s, reg)

	return mcpserver.ServeStdio(s)
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

func registerSearch(s *mcpserver.MCPServer, reg *collections.Registry) {
	s.AddTool(searchTool(reg), searchHandler(reg))
}

// searchTool builds the mk_search tool definition. Split out from
// registerSearch so its annotations (see the inline comment below) are
// directly unit-testable without standing up a server or driving stdio.
func searchTool(reg *collections.Registry) mcp.Tool {
	return mcp.NewTool("mk_search",
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
func searchHandler(reg *collections.Registry) mcpserver.ToolHandlerFunc {
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

		results, err := reg.Search(ctx, req.GetString("collection", ""), query, limit)
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

func registerShow(s *mcpserver.MCPServer, reg *collections.Registry) {
	s.AddTool(showTool(reg), showHandler(reg))
}

// showTool builds the mk_show tool definition. Split out from
// registerShow for the same reason as searchTool: direct annotation
// unit-testing.
func showTool(reg *collections.Registry) mcp.Tool {
	return mcp.NewTool("mk_show",
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
func showHandler(reg *collections.Registry) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ref, err := reg.Show(req.GetString("collection", ""), id)
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

func registerList(s *mcpserver.MCPServer, reg *collections.Registry) {
	s.AddTool(listTool(reg), listHandler(reg))
}

// listTool builds the mk_list tool definition. Split out from
// registerList for the same reason as searchTool: direct annotation
// unit-testing.
func listTool(reg *collections.Registry) mcp.Tool {
	return mcp.NewTool("mk_list",
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
func listHandler(reg *collections.Registry) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		refs, err := reg.Pages(req.GetString("collection", ""))
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

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
