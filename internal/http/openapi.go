package http

import (
	"strings"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// openAPISchema returns an OpenAPI 3.1 description of the meerkat
// tool endpoints. We hand-roll this rather than auto-generating
// because the surface is tiny and OpenWebUI is sensitive to small
// schema drifts.
//
// Reference: https://docs.openwebui.com/features/plugin/tools/openapi-tools/
func openAPISchema(version string, collectionNames []string) map[string]any {
	if version == "" {
		version = "dev"
	}

	bearerSecurity := []map[string]any{{"bearerAuth": []string{}}}

	// collectionProperty is the optional "collection" field every data
	// endpoint accepts. Its description carries the mounted names, so a
	// client that fetches the schema (OpenWebUI does) learns the set
	// without a second call.
	collectionDesc := "Optional. Restrict to one collection. "
	if len(collectionNames) > 1 {
		collectionDesc += "Mounted collections: " + strings.Join(collectionNames, ", ") +
			". Omit to span all of them; every result names the collection it came from."
	} else {
		collectionDesc += "This deployment mounts a single collection, so it can be omitted."
	}
	collectionProperty := map[string]any{"type": "string", "description": collectionDesc}

	paths := map[string]any{
		"/search": map[string]any{
			"post": map[string]any{
				"operationId": "mk_search",
				"summary":     "Full-text search across the knowledge base",
				"description": "Searches the embedded knowledge base for the given query. Title and ID matches are boosted so page-name lookups (e.g. a concept or service name) rank highest. Returns hits with score, snippet, category, and status.",
				"security":    bearerSecurity,
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type":     "object",
								"required": []string{"query"},
								"properties": map[string]any{
									"query": map[string]any{
										"type":        "string",
										"description": "Free-text query. Supports phrases ('like this'), field targeting (title:foo), and boolean operators.",
									},
									"limit": map[string]any{
										"type":        "integer",
										"description": "Maximum number of hits (default 10).",
										"default":     search.DefaultLimit,
										"minimum":     1,
										"maximum":     search.MaxLimit,
									},
									"collection": collectionProperty,
								},
							},
						},
					},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Search hits sorted by score, highest first.",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"id":         map[string]any{"type": "string"},
											"collection": map[string]any{"type": "string"},
											"title":      map[string]any{"type": "string"},
											"category":   map[string]any{"type": "string"},
											"status":     map[string]any{"type": "string"},
											"score":      map[string]any{"type": "number"},
											"snippet":    map[string]any{"type": "string"},
										},
									},
								},
							},
						},
					},
					"400": errorResponseSchema(),
					"401": errorResponseSchema(),
				},
			},
		},
		"/show": map[string]any{
			"post": map[string]any{
				"operationId": "mk_show",
				"summary":     "Retrieve a single knowledge-base page by ID",
				"description": "Returns the full markdown body and parsed frontmatter of one page, plus two OKF-derived advisory signals: trust_tier (unverified | machine-confirmed | human-reviewed, derived from front.verified — SPEC.md §5.3) and stale (whether today is on/after front.stale_after — SPEC.md §5.5). Page IDs are slash-separated paths from the wiki root without the .md suffix, e.g. 'concepts/Rate-Limiting' or 'systems/backend/auth-service'. Use POST /list first to discover valid IDs.",
				"security":    bearerSecurity,
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type":     "object",
								"required": []string{"id"},
								"properties": map[string]any{
									"id": map[string]any{
										"type":        "string",
										"description": "Page ID, e.g. 'concepts/Rate-Limiting'. May be qualified as '<collection>:<page-id>'.",
									},
									"collection": collectionProperty,
								},
							},
						},
					},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "The page exists; body and frontmatter are returned.",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"id":         map[string]any{"type": "string"},
										"collection": map[string]any{"type": "string"},
										"title":      map[string]any{"type": "string"},
										"body":       map[string]any{"type": "string"},
										"front":      map[string]any{"type": "object"},
										"trust_tier": map[string]any{
											"type":        "string",
											"description": "OKF advisory trust tier derived from front.verified (SPEC.md §5.3): unverified (no verified entries), machine-confirmed (verified, but no entry's by has the human: prefix), or human-reviewed (at least one verified entry's by has the human: prefix).",
											"enum":        []string{kb.TrustUnverified, kb.TrustMachineConfirmed, kb.TrustHumanReviewed},
										},
										"stale": map[string]any{
											"type":        "boolean",
											"description": "True once today is on/after front.stale_after (SPEC.md §5.5). False when stale_after is absent or unparsable — a missing optional field never manufactures a staleness signal.",
										},
									},
								},
							},
						},
					},
					"401": errorResponseSchema(),
					"404": errorResponseSchema(),
					// 409: the ID exists in several mounted collections and
					// none was named — the body lists the qualified IDs.
					"409": errorResponseSchema(),
				},
			},
		},
		"/list": map[string]any{
			"post": map[string]any{
				"operationId": "mk_list",
				"summary":     "List knowledge-base pages, optionally filtered",
				"description": "Returns every page in the embedded knowledge base. Filters compose (AND): prefix (page ID prefix), category ('systems', 'policies', 'adr', 'concepts', etc.), status ('placeholder', 'reviewed', 'stale'), owner, type (frontmatter 'type', OKF's concept-kind field, e.g. 'BigQuery Table', 'Metric', 'Playbook').",
				"security":    bearerSecurity,
				"requestBody": map[string]any{
					"required": false,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"prefix":     map[string]any{"type": "string", "description": "ID prefix filter, e.g. 'systems/backend/'."},
									"category":   map[string]any{"type": "string", "description": "Frontmatter category filter."},
									"status":     map[string]any{"type": "string", "description": "Frontmatter status filter."},
									"owner":      map[string]any{"type": "string", "description": "Frontmatter owner filter."},
									"type":       map[string]any{"type": "string", "description": "Frontmatter type filter (OKF's concept-kind field), e.g. 'BigQuery Table', 'Metric', 'Playbook'."},
									"collection": collectionProperty,
								},
							},
						},
					},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Page list sorted by ID.",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"id":         map[string]any{"type": "string"},
											"collection": map[string]any{"type": "string"},
											"title":      map[string]any{"type": "string"},
											"category":   map[string]any{"type": "string"},
											"status":     map[string]any{"type": "string"},
											"owner":      map[string]any{"type": "string"},
											"type":       map[string]any{"type": "string"},
											"source":     map[string]any{"type": "object"},
										},
									},
								},
							},
						},
					},
					"401": errorResponseSchema(),
				},
			},
		},
		"/collections": map[string]any{
			"get": map[string]any{
				"operationId": "mk_collections",
				"summary":     "List the knowledge-base collections this server mounts",
				"description": "Returns one entry per mounted collection: its name (what to pass as \"collection\" to /search, /show and /list), the content-source type behind it, its provenance, and how many pages it serves.",
				"security":    bearerSecurity,
				"responses": map[string]any{
					"200": map[string]any{
						"description": "The mounted collections, in configuration order.",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"name":   map[string]any{"type": "string"},
											"type":   map[string]any{"type": "string"},
											"source": map[string]any{"type": "string"},
											"pages":  map[string]any{"type": "integer"},
										},
									},
								},
							},
						},
					},
					"401": errorResponseSchema(),
				},
			},
		},
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Meerkat",
			"description": "Embedded knowledge base — search, show, list. All endpoints require an API key supplied via 'Authorization: Bearer <key>'. The key is configured server-side at startup; rotate by restarting.",
			"version":     version,
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "opaque",
					"description":  "Static bearer token configured server-side via --api-key or MEERKAT_API_KEY.",
				},
			},
		},
		"paths": paths,
	}
}

func errorResponseSchema() map[string]any {
	return map[string]any{
		"description": "Request rejected. Body contains an explanation.",
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"error": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}
