package http

// openAPISchema returns an OpenAPI 3.1 description of the meerkat
// tool endpoints. We hand-roll this rather than auto-generating
// because the surface is tiny and OpenWebUI is sensitive to small
// schema drifts.
//
// Reference: https://docs.openwebui.com/features/plugin/tools/openapi-tools/
func openAPISchema(version string) map[string]any {
	if version == "" {
		version = "dev"
	}

	bearerSecurity := []map[string]any{{"bearerAuth": []string{}}}

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
										"default":     10,
										"minimum":     1,
										"maximum":     100,
									},
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
											"id":       map[string]any{"type": "string"},
											"title":    map[string]any{"type": "string"},
											"category": map[string]any{"type": "string"},
											"status":   map[string]any{"type": "string"},
											"score":    map[string]any{"type": "number"},
											"snippet":  map[string]any{"type": "string"},
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
				"description": "Returns the full markdown body and parsed frontmatter of one page. Page IDs are slash-separated paths from the wiki root without the .md suffix, e.g. 'concepts/Rate-Limiting' or 'systems/backend/auth-service'. Use POST /list first to discover valid IDs.",
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
										"description": "Page ID, e.g. 'concepts/Rate-Limiting'",
									},
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
										"id":    map[string]any{"type": "string"},
										"title": map[string]any{"type": "string"},
										"body":  map[string]any{"type": "string"},
										"front": map[string]any{"type": "object"},
									},
								},
							},
						},
					},
					"401": errorResponseSchema(),
					"404": errorResponseSchema(),
				},
			},
		},
		"/list": map[string]any{
			"post": map[string]any{
				"operationId": "mk_list",
				"summary":     "List knowledge-base pages, optionally filtered",
				"description": "Returns every page in the embedded knowledge base. Filters compose (AND): prefix (page ID prefix), category ('systems', 'policies', 'adr', 'concepts', etc.), status ('placeholder', 'reviewed', 'stale'), owner.",
				"security":    bearerSecurity,
				"requestBody": map[string]any{
					"required": false,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"prefix":   map[string]any{"type": "string", "description": "ID prefix filter, e.g. 'systems/backend/'."},
									"category": map[string]any{"type": "string", "description": "Frontmatter category filter."},
									"status":   map[string]any{"type": "string", "description": "Frontmatter status filter."},
									"owner":    map[string]any{"type": "string", "description": "Frontmatter owner filter."},
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
											"id":       map[string]any{"type": "string"},
											"title":    map[string]any{"type": "string"},
											"category": map[string]any{"type": "string"},
											"status":   map[string]any{"type": "string"},
											"owner":    map[string]any{"type": "string"},
											"source":   map[string]any{"type": "object"},
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
