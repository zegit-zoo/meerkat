// Package mcp wires the embedded knowledge base to a Model Context
// Protocol server. The server exposes:
//
//	mk_search - full-text search across all wiki pages
//	mk_show   - retrieve a single wiki page by ID
//	mk_list   - enumerate wiki pages with optional filters
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

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// ServeStdio constructs an MCP server with the meerkat KB tool set
// and serves it on stdio until ctx is cancelled or stdin closes.
//
// The search index is built once at startup and reused across calls.
// Bleve in-memory indexes are safe for concurrent reads.
func ServeStdio(ctx context.Context) error {
	idx, err := search.New()
	if err != nil {
		return fmt.Errorf("build search index: %w", err)
	}
	defer idx.Close()

	s := mcpserver.NewMCPServer(
		"meerkat",
		"0.2.0",
		mcpserver.WithToolCapabilities(true),
	)

	registerSearch(s, idx)
	registerShow(s)
	registerList(s)

	return mcpserver.ServeStdio(s)
}

func registerSearch(s *mcpserver.MCPServer, idx *search.Index) {
	tool := mcp.NewTool("mk_search",
		mcp.WithDescription(
			"Full-text search across the knowledge base "+
				"(meerkat). Title and ID matches are boosted so "+
				"page-name lookups (e.g. a concept or service name) "+
				"rank highest. Returns a JSON list of "+
				"{id, title, score, snippet, category} hits."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Free-text query. Supports phrases ('like this'), field targeting (title:foo), and boolean operators."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results (default 10)."),
		),
	)

	s.AddTool(tool, searchHandler(idx))
}

// searchHandler returns the mk_search tool handler bound to idx. Split out
// (and the formatting moved to searchResultsJSON) so the query→result-shape
// path is unit-testable against an injected index.
func searchHandler(idx *search.Index) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		limit := 10
		if v := req.GetFloat("limit", 0); v > 0 {
			limit = int(v)
		}
		results, err := idx.Query(query, limit)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		body, err := searchResultsJSON(results)
		if err != nil {
			return nil, fmt.Errorf("encode results: %w", err)
		}
		return mcp.NewToolResultText(body), nil
	}
}

// searchResultsJSON renders search results into the stable mk_search wire
// shape: a JSON array of {id, title, category, status, score, snippet}.
func searchResultsJSON(results []search.Result) (string, error) {
	simplified := make([]map[string]any, len(results))
	for i, r := range results {
		simplified[i] = map[string]any{
			"id":       r.Page.ID,
			"title":    r.Page.Title,
			"category": r.Page.Front.Category,
			"status":   r.Page.Front.Status,
			"score":    r.Score,
			"snippet":  oneLine(r.Snippet),
		}
	}
	body, err := json.MarshalIndent(simplified, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func registerShow(s *mcpserver.MCPServer) {
	tool := mcp.NewTool("mk_show",
		mcp.WithDescription(
			"Retrieve a single knowledge-base page by ID. Page IDs are "+
				"slash-separated paths from the wiki root without "+
				"the .md suffix, e.g. 'concepts/Some-Concept', "+
				"'systems/backend/some-service'. Returns a JSON "+
				"object {id, title, body, front} where front is the "+
				"parsed frontmatter (category, owner, status, "+
				"source, related, tags)."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("Page ID, e.g. 'concepts/Some-Concept'"),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		page, err := kb.Load(id)
		if err != nil {
			if errors.Is(err, kb.ErrNotFound) {
				return mcp.NewToolResultError(
					fmt.Sprintf("page %q not found - try the mk_list tool", id),
				), nil
			}
			return nil, err
		}
		body, err := json.MarshalIndent(page, "", "  ")
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(body)), nil
	})
}

func registerList(s *mcpserver.MCPServer) {
	tool := mcp.NewTool("mk_list",
		mcp.WithDescription(
			"List knowledge-base pages, optionally filtered. Filters "+
				"compose (AND): prefix (page ID prefix), category "+
				"(frontmatter category, e.g. 'policies', 'systems', "+
				"'adr', 'concepts'), status (e.g. 'placeholder', "+
				"'reviewed'), owner. Returns "+
				"[{id, title, category, status, owner, source}]."),
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
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		pages, err := kb.List()
		if err != nil {
			return nil, err
		}
		pages = filterPages(pages,
			req.GetString("prefix", ""),
			req.GetString("category", ""),
			req.GetString("status", ""),
			req.GetString("owner", ""),
		)
		body, err := listPagesJSON(pages)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(body), nil
	})
}

// filterPages applies the composable (AND) mk_list filters. Empty filters
// are no-ops. Split out so the filter composition is unit-testable without
// the embedded KB.
func filterPages(pages []kb.Page, prefix, category, status, owner string) []kb.Page {
	if prefix != "" {
		pages = kb.Filter(pages, kb.ByPrefix(prefix))
	}
	if category != "" {
		pages = kb.Filter(pages, kb.ByCategory(category))
	}
	if status != "" {
		pages = kb.Filter(pages, kb.ByStatus(status))
	}
	if owner != "" {
		pages = kb.Filter(pages, kb.ByOwner(owner))
	}
	return pages
}

// listPagesJSON renders the mk_list wire shape: a JSON array of
// {id, title, category, status, owner, source}.
func listPagesJSON(pages []kb.Page) (string, error) {
	out := make([]map[string]any, 0, len(pages))
	for _, p := range pages {
		out = append(out, map[string]any{
			"id":       p.ID,
			"title":    p.Title,
			"category": p.Front.Category,
			"status":   p.Front.Status,
			"owner":    p.Front.Owner,
			"source":   p.Front.Source,
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
