package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// limits.go bounds what one request, and the set of requests in flight,
// can cost the hosted server (meerkat-mob#41).
//
// mcp-go's Streamable HTTP handler reads a POST body in full before it
// looks at anything else, so the transport itself has no notion of "too
// big". The bound therefore has to sit in front of it:
//
//   - The body cap answers 413 to a request that DECLARES a body over the
//     cap before a single byte of it is read, and caps a request that does
//     not declare its length (chunked) at the same number of bytes: it is
//     read up to the cap and refused at cap+1. Nothing past the cap is
//     ever buffered.
//   - The in-flight caps bound how many requests, and so how many body
//     buffers and tool calls behind them, exist at once. Short requests
//     (POST, DELETE, ...) and long-lived SSE streams (GET) have separate
//     pools: a stream holds its slot for the whole session, and letting
//     streams use up the request pool would let idle clients lock out
//     every call. A request that finds its pool full is answered 503 with
//     Retry-After at once rather than queued; a queue would only move the
//     memory from the handler to the accept loop.
//
// The order on the MCP endpoint is load-bearing (HostedServer.routes):
//
//	inflight -> limitBody -> authentication gate -> bufferBody -> transport
//
// The in-flight cap is outermost, so every request holds a slot before
// it costs anything, and a buffer can only exist inside a slot. limitBody
// refuses a declared oversize body and wraps the rest in the cap, but
// reads nothing. The gate then decides on headers alone, and only a
// caller it admits reaches bufferBody, which is where a body of unknown
// length is read: an unauthenticated request never has its body
// buffered here. Both pools are shared by authenticated and
// unauthenticated callers, so they bound the server's total cost, not
// any one caller's share of it.

// Defaults for the request bounds. The body cap is well above the
// largest legitimate call — a memory save, whose content is capped at
// 256 KiB, JSON-escaped, inside a JSON-RPC envelope — and well below
// anything that hurts a process.
const (
	DefaultMaxRequestBytes       int64 = 4 << 20
	DefaultMaxConcurrentRequests       = 128
	DefaultMaxConcurrentStreams        = 512
)

// limitBody refuses a request body over max bytes with 413.
//
// A declared Content-Length over the cap is refused before the body is
// touched. Every other body is wrapped in http.MaxBytesReader, so
// whoever reads it later — bufferBody, or the transport — cannot read
// past the cap, including from a client that sends more than it
// declared. limitBody itself reads nothing: it runs before the
// authentication gate, where work must stay cheap.
func limitBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > max {
			tooLarge(w, max)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

// bufferBody reads a body of unknown length (chunked) in full, so that
// an overrun of the cap limitBody set is answered 413 by this server
// rather than as whatever parse error the transport would make of a
// truncated read. It buffers at most max bytes, and it sits behind the
// authentication gate, so only an admitted caller's body is ever
// buffered. A body of declared length is left to the transport, which
// reads it through the same cap.
func bufferBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody || r.ContentLength >= 0 {
			next.ServeHTTP(w, r)
			return
		}
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				tooLarge(w, max)
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(buf))
		r.ContentLength = int64(len(buf))
		next.ServeHTTP(w, r)
	})
}

// tooLarge answers 413 and closes the connection: the unread rest of
// the body is not worth draining.
func tooLarge(w http.ResponseWriter, max int64) {
	w.Header().Set("Connection", "close")
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
		"error": fmt.Sprintf("request body exceeds the %d-byte limit", max),
	})
}

// inflight caps concurrent requests on the MCP endpoint, in two pools.
// A nil channel is an unlimited pool.
type inflight struct {
	requests chan struct{}
	streams  chan struct{}
}

// newInflight builds the caps. A value <= 0 leaves that pool unlimited;
// HostedConfig.applyDefaults has already turned 0 into the default, so
// only an explicit negative gets here as "off".
func newInflight(requests, streams int) *inflight {
	l := &inflight{}
	if requests > 0 {
		l.requests = make(chan struct{}, requests)
	}
	if streams > 0 {
		l.streams = make(chan struct{}, streams)
	}
	return l
}

// middleware admits a request into its pool or answers 503.
func (l *inflight) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pool := l.requests
		if r.Method == http.MethodGet {
			pool = l.streams
		}
		if pool == nil {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case pool <- struct{}{}:
			defer func() { <-pool }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "server busy; retry shortly"})
		}
	})
}
