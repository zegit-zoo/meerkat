package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// outcome.go is mk_report_outcome (meerkat-mob issue G): the tool that
// tells meerkat whether a retrieval helped, and — when the agent gave
// up and did its own research — turns that into the knowledge base's
// next page.
//
// Three things happen on a report, each independently opt-in by
// configuration and each visible in the response:
//
//  1. Telemetry, always: a span and meerkat_retrieval_outcomes_total
//     carry the outcome and the fallback KIND as closed-set values, plus
//     counts. Never the query, never a page ID, never a collection name.
//  2. The traversal log, when observability.traversal_log is set: one
//     object per report with HMAC-hashed page IDs and collection names,
//     the path shape (tree depths), the quality scores and the initial
//     query in plaintext. See internal/traversal.
//  3. The intake store, when intake: is set, the caller holds
//     intake-write, and fallback.kind is not "none": the agent's
//     research becomes a raw page (type: research-raw, status:
//     unverified, source: agent-fallback) for the librarian pipeline
//     (issue H) to validate and place.

const toolReportOutcome = "mk_report_outcome"

// Outcome values.
const (
	OutcomeFound    = "found"
	OutcomeNotFound = "not_found"
	OutcomeGaveUp   = "gave_up"
)

// Fallback kinds.
const (
	FallbackNone   = "none"
	FallbackWeb    = "web"
	FallbackSource = "source"
	FallbackHuman  = "human"
)

// Bounds on a report. The initial query is stored verbatim, so it gets
// a generous cap; lists are bounded so a runaway agent cannot turn one
// report into a megabyte.
const (
	maxInitialQueryBytes = 2048
	maxSummaryBytes      = 16 << 10
	maxNotesBytes        = 2048
	maxListItems         = 50
	maxSourceItems       = 20
	maxSessionIDLen      = 128
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// OutcomeOptions is what the tool needs beyond the registry. Zero
// values mean "not configured": the tool still records telemetry and
// says so in its response.
type OutcomeOptions struct {
	// Log is the traversal log; nil logs nothing.
	Log *traversal.Log
	// Intake is the store fallback research is written to; nil writes
	// nothing.
	Intake memory.Store
}

type outcomeArgs struct {
	sessionID    string
	outcome      string
	initialQuery string
	pages        []string
	attempted    []string
	quality      *traversal.Quality
	fallback     traversal.Fallback
}

func registerReportOutcome(s *mcpserver.MCPServer, reg *collections.Registry, opts transportOptions) {
	s.AddTool(reportOutcomeTool(reg), reportOutcomeHandler(reg, opts))
}

func reportOutcomeTool(reg *collections.Registry) mcp.Tool {
	mounted := " Mounted collections: " + strings.Join(reg.Names(), ", ") + "."
	if reg.Single() {
		mounted = " This server currently mounts a single collection (" + reg.Names()[0] + ")."
	}
	return mcp.NewTool(toolReportOutcome,
		mcp.WithDescription(
			"Report how a retrieval went. If meerkat did not have what you needed, report it "+
				"here with what you found instead: every report improves the next agent's retrieval, "+
				"so report even when there is no direct benefit to your current task. "+
				"outcome is found | not_found | gave_up. initial_query is your FIRST query, verbatim "+
				"(it is how the librarians learn what agents ask and how routing failed). pages are the "+
				"page IDs that actually answered ('collection:id'); attempted are the collections you "+
				"searched, in order. quality scores (0..1) say how accurate, complete and useful the "+
				"answer was. fallback says what you did instead — kind web | source | human | none, a "+
				"summary of what you learned, and the sources — and when the deployment has an intake "+
				"store and you hold intake-write, that summary becomes a draft page for review. "+
				"Returns {recorded, logged, intake_id?}. Idempotent per session_id: report once at the "+
				"end; a later report for the same session supersedes it."+
				mounted),
		mcp.WithString("session_id", mcp.Description("The retrieval session this report closes. Optional; defaults to the MCP session, or a fresh ID for a stateless caller. Pass the same value you gave mk_search.")),
		mcp.WithString("outcome", mcp.Required(), mcp.Description("found | not_found | gave_up")),
		mcp.WithString("initial_query", mcp.Description("Your first query, exactly as you sent it (max 2 KiB).")),
		mcp.WithArray("pages", mcp.Description("Qualified page IDs ('collection:id') that answered, if any."), mcp.WithStringItems()),
		mcp.WithArray("attempted", mcp.Description("Collections searched, in order (names or tree paths)."), mcp.WithStringItems()),
		mcp.WithObject("quality", mcp.Description("{accuracy, completeness, answer_quality: 0..1, notes?}")),
		mcp.WithObject("fallback", mcp.Description("{kind: web|source|human|none, summary?, sources?: [urls]}")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)
}

func parseOutcomeArgs(req mcp.CallToolRequest) (outcomeArgs, error) {
	var a outcomeArgs
	var err error
	if a.outcome, err = req.RequireString("outcome"); err != nil {
		return a, err
	}
	switch a.outcome {
	case OutcomeFound, OutcomeNotFound, OutcomeGaveUp:
	default:
		return a, fmt.Errorf("outcome must be %s, %s or %s, got %q", OutcomeFound, OutcomeNotFound, OutcomeGaveUp, a.outcome)
	}
	a.sessionID = strings.TrimSpace(req.GetString("session_id", ""))
	if a.sessionID != "" && (len(a.sessionID) > maxSessionIDLen || !sessionIDPattern.MatchString(a.sessionID)) {
		return a, fmt.Errorf("session_id must be 1–%d characters of [A-Za-z0-9._:-]", maxSessionIDLen)
	}
	a.initialQuery = strings.TrimSpace(req.GetString("initial_query", ""))
	if len(a.initialQuery) > maxInitialQueryBytes {
		return a, fmt.Errorf("initial_query is %d bytes, over the %d-byte cap", len(a.initialQuery), maxInitialQueryBytes)
	}
	if a.pages, err = stringList(req, "pages", maxListItems); err != nil {
		return a, err
	}
	if a.attempted, err = stringList(req, "attempted", maxListItems); err != nil {
		return a, err
	}
	if q, ok := req.GetArguments()["quality"]; ok && q != nil {
		m, ok := q.(map[string]any)
		if !ok {
			return a, errors.New("quality must be an object {accuracy, completeness, answer_quality, notes?}")
		}
		var qu traversal.Quality
		for name, dst := range map[string]*float64{"accuracy": &qu.Accuracy, "completeness": &qu.Completeness, "answer_quality": &qu.AnswerQuality} {
			v, ok := m[name]
			if !ok {
				return a, fmt.Errorf("quality.%s is required (0..1)", name)
			}
			f, ok := asFloat(v)
			if !ok || f < 0 || f > 1 {
				return a, fmt.Errorf("quality.%s must be a number between 0 and 1", name)
			}
			*dst = f
		}
		if n, ok := m["notes"].(string); ok {
			if len(n) > maxNotesBytes {
				return a, fmt.Errorf("quality.notes is %d bytes, over the %d-byte cap", len(n), maxNotesBytes)
			}
			qu.Notes = n
		}
		a.quality = &qu
	}
	a.fallback = traversal.Fallback{Kind: FallbackNone}
	if f, ok := req.GetArguments()["fallback"]; ok && f != nil {
		m, ok := f.(map[string]any)
		if !ok {
			return a, errors.New("fallback must be an object {kind, summary?, sources?}")
		}
		kind, _ := m["kind"].(string)
		switch kind {
		case FallbackNone, FallbackWeb, FallbackSource, FallbackHuman:
		case "":
			return a, errors.New("fallback.kind is required: web | source | human | none")
		default:
			return a, fmt.Errorf("fallback.kind must be web, source, human or none, got %q", kind)
		}
		a.fallback.Kind = kind
		if s, ok := m["summary"].(string); ok {
			if len(s) > maxSummaryBytes {
				return a, fmt.Errorf("fallback.summary is %d bytes, over the %d-byte cap", len(s), maxSummaryBytes)
			}
			a.fallback.Summary = strings.TrimSpace(s)
		}
		if raw, ok := m["sources"].([]any); ok {
			if len(raw) > maxSourceItems {
				return a, fmt.Errorf("fallback.sources has %d entries, over the %d cap", len(raw), maxSourceItems)
			}
			for _, r := range raw {
				s, ok := r.(string)
				if !ok || strings.TrimSpace(s) == "" {
					return a, errors.New("fallback.sources must be strings")
				}
				if u, err := url.Parse(s); err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "git" && u.Scheme != "file") {
					return a, fmt.Errorf("fallback.sources entry %q is not an http(s)/git/file URL", s)
				}
				a.fallback.Sources = append(a.fallback.Sources, s)
			}
		}
		if a.fallback.Kind != FallbackNone && a.fallback.Summary == "" {
			return a, fmt.Errorf("fallback.summary is required when fallback.kind is %q — it is what becomes the draft page", a.fallback.Kind)
		}
	}
	return a, nil
}

// asFloat accepts the number shapes a JSON decoder or a Go caller may
// hand over.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func stringList(req mcp.CallToolRequest, name string, cap int) ([]string, error) {
	raw, ok := req.GetArguments()[name]
	if !ok || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
	if len(items) > cap {
		return nil, fmt.Errorf("%s has %d entries, over the %d cap", name, len(items), cap)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%s must be non-empty strings", name)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

func reportOutcomeHandler(reg *collections.Registry, opts transportOptions) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseOutcomeArgs(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if args.sessionID == "" {
			args.sessionID = sessionIDFrom(ctx)
		}
		g := authz.FromContext(ctx)
		view := visible(ctx, reg, opts)

		// Path shape: the tree depth of each attempted collection this
		// caller can see; -1 for anything else. Depths are numbers and
		// may reach spans; names may not.
		shape := make([]int, len(args.attempted))
		tierReached := -1
		for i, name := range args.attempted {
			shape[i] = -1
			if n, ok := view.TreeNode(name); ok {
				shape[i] = n.Depth
			} else if strings.Contains(name, "/") {
				if c, err := view.Get(name); err == nil && c.Tree != nil {
					shape[i] = c.Tree.Depth
				}
			}
			if shape[i] > tierReached {
				tierReached = shape[i]
			}
		}

		ctx, span := telemetry.Span(ctx, telemetry.SpanOutcomeReport,
			telemetry.KeyOutcomeResult.String(args.outcome),
			telemetry.KeyOutcomeFallback.String(args.fallback.Kind),
			telemetry.KeyOutcomePages.Int(len(args.pages)),
			telemetry.KeyOutcomeHops.Int(len(args.attempted)),
			telemetry.KeyOutcomeTierReached.Int(tierReached),
			telemetry.KeyOutcomeHasQuality.Bool(args.quality != nil),
		)
		started := time.Now()

		resp := map[string]any{"recorded": true, "logged": false, "intake": "not_configured"}

		// 3. Intake first, so the log line can carry the intake ID.
		intakeID := ""
		if args.fallback.Kind != FallbackNone {
			switch {
			case opts.Outcome.Intake == nil:
				resp["intake"] = "not_configured"
			case !g.CanIntake():
				resp["intake"] = "not_permitted"
			default:
				id, err := writeIntake(ctx, opts.Outcome.Intake, g, args, started)
				if err != nil {
					telemetry.Record(ctx).RetrievalOutcome(args.outcome, args.fallback.Kind, telemetry.OutcomeError)
					telemetry.Fail(span, telemetry.OutcomeError)
					return mcp.NewToolResultError("intake: " + err.Error()), nil
				}
				intakeID = id
				resp["intake"] = "written"
				resp["intake_id"] = id
			}
		} else {
			resp["intake"] = "none"
		}

		// 2. The traversal log.
		if opts.Outcome.Log.Enabled() {
			entry := traversal.Entry{
				Session:      args.sessionID,
				Outcome:      args.outcome,
				InitialQuery: args.initialQuery,
				Pages:        append([]string(nil), args.pages...),
				Attempted:    append([]string(nil), args.attempted...),
				PathShape:    shape,
				TierReached:  tierReached,
				Hops:         len(args.attempted),
				Quality:      args.quality,
				IntakeID:     intakeID,
			}
			if args.fallback.Kind != FallbackNone {
				fb := args.fallback
				entry.Fallback = &fb
			}
			if _, err := opts.Outcome.Log.Record(ctx, entry); err != nil {
				telemetry.Record(ctx).RetrievalOutcome(args.outcome, args.fallback.Kind, telemetry.OutcomeError)
				telemetry.Fail(span, telemetry.OutcomeError)
				return mcp.NewToolResultError("traversal log: " + err.Error()), nil
			}
			resp["logged"] = true
		}

		// 1. Telemetry, always.
		telemetry.Record(ctx).RetrievalOutcome(args.outcome, args.fallback.Kind, telemetry.OutcomeOK)
		span.SetAttributes(telemetry.Outcome(telemetry.OutcomeOK))
		span.End()
		return jsonResult(resp)
	}
}

// sessionIDFrom returns the MCP client session's ID, or a fresh one
// for a stateless caller.
func sessionIDFrom(ctx context.Context) string {
	if cs := mcpserver.ClientSessionFromContext(ctx); cs != nil && cs.SessionID() != "" {
		return cs.SessionID()
	}
	return "anon-" + newID()
}

// newID returns 16 random hex characters: an intake object key, or a
// session ID for a caller that has none.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// writeIntake stores the fallback research as a raw page and returns
// its key. The page is a markdown document with the frontmatter the
// librarian pipeline (issue H) expects; it is written create-only under
// a unique key, so it is single-writer on every provider.
func writeIntake(ctx context.Context, store memory.Store, g *authz.Grants, a outcomeArgs, now time.Time) (string, error) {
	id := newID()
	key := "raw/" + now.UTC().Format("2006-01-02") + "/" + id + ".md"
	front := map[string]any{
		"id":               id,
		"type":             "research-raw",
		"status":           "unverified",
		"source":           "agent-fallback",
		"outcome":          a.outcome,
		"fallback_kind":    a.fallback.Kind,
		"question":         a.initialQuery,
		"attempted":        a.attempted,
		"reported_at":      now.UTC().Format(time.RFC3339),
		"session_id":       a.sessionID,
		"submitted_by":     memory.Namespace(g.Identity()),
		"fallback_sources": a.fallback.Sources,
	}
	if a.quality != nil {
		front["quality"] = map[string]any{"accuracy": a.quality.Accuracy, "completeness": a.quality.Completeness, "answer_quality": a.quality.AnswerQuality}
	}
	fm, err := json.Marshal(front)
	if err != nil {
		return "", err
	}
	// JSON is valid YAML: a one-object frontmatter block that any YAML
	// consumer (and the OKF loader) reads, with no quoting rules to get
	// wrong for free text.
	body := "---\n" + string(fm) + "\n---\n# " + titleFor(a) + "\n\n" + a.fallback.Summary + "\n"
	if len(a.fallback.Sources) > 0 {
		body += "\n## Sources\n"
		for _, s := range a.fallback.Sources {
			body += "- " + s + "\n"
		}
	}
	if _, err := store.Put(ctx, key, []byte(body), memory.CreateOnly()); err != nil {
		return "", err
	}
	return key, nil
}

func titleFor(a outcomeArgs) string {
	if a.initialQuery != "" {
		q := a.initialQuery
		if len(q) > 80 {
			q = q[:80] + "…"
		}
		return "Research: " + strings.Join(strings.Fields(q), " ")
	}
	return "Research from an agent fallback (" + a.fallback.Kind + ")"
}
