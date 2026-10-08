package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/retrieval"
)

// session.go scopes every session-shaped identifier to the principal
// that holds it (meerkat-mob#46).
//
// Two kinds of identifier reach the server from the caller:
//
//   - the session_id tool argument, which keys retrieval sessions,
//     their traversal limits, and freshness advisories; and
//   - the MCP session ID (the Mcp-Session-Id header), which keys the
//     transport's per-session state, the GET stream above all.
//
// Both are strings the caller chooses or can repeat, and neither used to
// say whose it was. Now:
//
//   - A tool-level session key is principal + "\x00" + id (sessionKey,
//     advisoryKey), so equal ids from different principals are different
//     sessions.
//   - An MCP session ID carries a tag derived from the principal that was
//     issued it, and a request presenting an ID whose tag does not match
//     its own principal is answered 404 — as an unknown session — on
//     POST, GET and DELETE alike, in stateless and stateful mode. The tag
//     needs no secret: it is checked against the principal of the request
//     presenting the ID, which comes from a verified token, so knowing
//     another principal's ID — or computing a tag for it — does not make
//     it yours. Nothing is stored for this in stateless mode, so replicas
//     still need no sticky routing.
//
// The principal is memory.Namespace(identity): a hash of the token's
// (iss, sub), the same value that owns personal memories. Every caller
// with no subject — anonymous callers, and every caller of a deployment
// without auth — is one principal, so such callers are separated from
// authenticated ones, not from each other.

// principal is the session-scoping principal of the caller in ctx.
func principal(ctx context.Context) string {
	return memory.Namespace(authz.FromContext(ctx).Identity())
}

// sessionKey is the retrieval-session key for a call: the explicit
// session_id, else the MCP client session, else "" (no session) —
// scoped to the caller's principal.
func sessionKey(ctx context.Context, explicit string) string {
	id := explicit
	if id == "" {
		if cs := mcpserver.ClientSessionFromContext(ctx); cs != nil {
			id = cs.SessionID()
		}
	}
	return retrieval.Key(principal(ctx), id)
}

// advisoryKey is the freshness-advisory key for a call. Unlike
// sessionKey it is never empty: a call with no session shares one
// advisory bucket with the rest of its principal's session-less calls,
// and with nobody else's.
func advisoryKey(ctx context.Context, explicit string) string {
	if k := sessionKey(ctx, explicit); k != "" {
		return k
	}
	return principal(ctx) + "\x00"
}

// --- MCP session IDs --------------------------------------------------------

// sessionIDPrefix keeps mcp-go's ID shape recognisable in logs.
const sessionIDPrefix = "mcp-session-"

// Defaults for the MCP session caps.
const (
	// DefaultMaxSessionsPerPrincipal caps the session IDs one principal
	// holds in stateful mode, and the GET streams it holds open in either
	// mode.
	DefaultMaxSessionsPerPrincipal = 32
	// DefaultMaxSessions caps the session IDs held in stateful mode
	// overall. (Open streams overall are the endpoint's concern.)
	DefaultMaxSessions = 10000
)

var errUnknownSession = errors.New("session not found")

// sessionTag binds nonce to principal.
func sessionTag(principal, nonce string) string {
	sum := sha256.Sum256([]byte("meerkat mcp session\x00" + principal + "\x00" + nonce))
	return hex.EncodeToString(sum[:12])
}

// newSessionID mints an MCP session ID bound to principal.
func newSessionID(principal string) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail (Go 1.24+)
	nonce := hex.EncodeToString(b[:])
	return sessionIDPrefix + nonce + "." + sessionTag(principal, nonce)
}

// boundTo reports whether id is a well-formed session ID bound to
// principal.
func boundTo(id, principal string) bool {
	rest, ok := strings.CutPrefix(id, sessionIDPrefix)
	if !ok {
		return false
	}
	nonce, tag, ok := strings.Cut(rest, ".")
	if !ok || len(nonce) != 32 || len(tag) != 24 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tag), []byte(sessionTag(principal, nonce))) == 1
}

// sessionBinder is the MCP session ID policy: it mints principal-bound
// IDs (as mcp-go's SessionIdManagerResolver), refuses a foreign one (as
// HTTP middleware, because mcp-go does not validate the ID on GET), and
// caps live sessions per principal and, in stateful mode, overall.
type sessionBinder struct {
	stateful        bool
	maxPerPrincipal int
	maxTotal        int
	now             func() time.Time

	mu sync.Mutex
	// ids is the stateful-mode registry: issued, unterminated IDs.
	ids map[string]issued
	// held counts stateful IDs per principal.
	held map[string]int
	// streams counts open GET streams per principal.
	streams map[string]int
}

type issued struct {
	principal string
	at        time.Time
}

func newSessionBinder(stateful bool, maxPerPrincipal, maxTotal int) *sessionBinder {
	return &sessionBinder{
		stateful: stateful, maxPerPrincipal: maxPerPrincipal, maxTotal: maxTotal, now: time.Now,
		ids: map[string]issued{}, held: map[string]int{}, streams: map[string]int{},
	}
}

// ResolveSessionIdManager implements mcpserver.SessionIdManagerResolver.
// A nil request is mcp-go's idle sweeper, which only terminates.
func (b *sessionBinder) ResolveSessionIdManager(r *http.Request) mcpserver.SessionIdManager {
	m := &boundManager{b: b}
	if r != nil {
		m.principal, m.scoped = principal(r.Context()), true
	}
	return m
}

// boundManager is one request's view of the binder.
type boundManager struct {
	b         *sessionBinder
	principal string
	scoped    bool
}

func (m *boundManager) Generate() string {
	id := newSessionID(m.principal)
	if m.b.stateful {
		m.b.issue(id, m.principal)
	}
	return id
}

func (m *boundManager) Validate(id string) (bool, error) {
	if !m.scoped || !boundTo(id, m.principal) {
		return false, errUnknownSession
	}
	if m.b.stateful && !m.b.known(id) {
		return false, errUnknownSession
	}
	return false, nil
}

func (m *boundManager) Terminate(id string) (bool, error) {
	// The sweeper (unscoped) may end any session; a request only its own.
	// The middleware has already refused a foreign ID, so the second
	// check is belt and braces.
	if m.scoped && !boundTo(id, m.principal) {
		return false, nil
	}
	m.b.forget(id)
	return false, nil
}

// issue registers a stateful ID, evicting the principal's oldest, then
// the oldest overall, to stay inside the caps. An evicted ID is answered
// as unknown from then on, and its client initializes again.
func (b *sessionBinder) issue(id, principal string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxPerPrincipal > 0 && b.held[principal] >= b.maxPerPrincipal {
		b.evictOldest(principal, true)
	}
	if b.maxTotal > 0 && len(b.ids) >= b.maxTotal {
		b.evictOldest("", false)
	}
	b.ids[id] = issued{principal: principal, at: b.now()}
	b.held[principal]++
}

func (b *sessionBinder) evictOldest(principal string, scoped bool) {
	var oldID string
	var old issued
	for id, is := range b.ids {
		if scoped && is.principal != principal {
			continue
		}
		if oldID == "" || is.at.Before(old.at) {
			oldID, old = id, is
		}
	}
	if oldID != "" {
		b.forgetLocked(oldID)
	}
}

func (b *sessionBinder) known(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.ids[id]
	return ok
}

func (b *sessionBinder) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forgetLocked(id)
}

func (b *sessionBinder) forgetLocked(id string) {
	is, ok := b.ids[id]
	if !ok {
		return
	}
	delete(b.ids, id)
	if b.held[is.principal] <= 1 {
		delete(b.held, is.principal)
	} else {
		b.held[is.principal]--
	}
}

// openStream admits one more GET stream for principal, or reports that
// it already holds its cap.
func (b *sessionBinder) openStream(principal string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxPerPrincipal > 0 && b.streams[principal] >= b.maxPerPrincipal {
		return false
	}
	b.streams[principal]++
	return true
}

func (b *sessionBinder) closeStream(principal string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.streams[principal] <= 1 {
		delete(b.streams, principal)
	} else {
		b.streams[principal]--
	}
}

// middleware refuses a request that presents another principal's MCP
// session ID, and caps a principal's open GET streams. It runs below the
// authentication gate, so the principal is the verified one.
//
// A foreign or malformed ID is answered exactly as mcp-go answers an
// unknown one (404), so the answer says nothing about whether the ID is
// live for somebody else.
func (b *sessionBinder) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r.Context())
		if id := r.Header.Get(mcpserver.HeaderKeySessionID); id != "" && !boundTo(id, p) {
			http.Error(w, "Invalid session ID", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			if !b.openStream(p) {
				w.Header().Set("Retry-After", "5")
				http.Error(w, "too many open streams for this caller", http.StatusTooManyRequests)
				return
			}
			defer b.closeStream(p)
		}
		next.ServeHTTP(w, r)
	})
}
