package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
)

// limits_test.go: the hosted MCP endpoint bounds what one request and the
// requests in flight can cost (meerkat-mob#41), and its HTTP metrics have
// a closed label set whatever a client sends (meerkat-mob#47).

// newLimitServer builds an unauthenticated hosted server over the
// three-collection fixture with cfg's limits applied.
func newLimitServer(t *testing.T, cfg HostedConfig) *HostedServer {
	t.Helper()
	cfg.Collections = threeCollectionRegistry(t)
	cfg.Version = "test"
	cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv, err := NewHosted(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// countingReader yields n bytes of JSON-ish filler and counts what was
// actually read from it.
type countingReader struct {
	left int64
	read atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.left {
		n = r.left
	}
	for i := range p[:n] {
		p[i] = ' '
	}
	r.left -= n
	r.read.Add(n)
	return int(n), nil
}

func TestHostedLimits_DeclaredOversizedBodyIs413WithoutBeingRead(t *testing.T) {
	srv := newLimitServer(t, HostedConfig{})
	const size = 10 << 20
	body := &countingReader{left: size}
	req := httptest.NewRequest(http.MethodPost, srv.EndpointPath(), body)
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if n := body.read.Load(); n != 0 {
		t.Errorf("%d bytes of a body declared over the cap were read; want none", n)
	}
}

func TestHostedLimits_UndeclaredOversizedBodyIsCappedAnd413(t *testing.T) {
	const limit = 64 << 10
	srv := newLimitServer(t, HostedConfig{MaxRequestBytes: limit})
	body := &countingReader{left: 10 << 20}
	req := httptest.NewRequest(http.MethodPost, srv.EndpointPath(), body)
	req.ContentLength = -1 // chunked: the length is not declared
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	// MaxBytesReader reads at most one buffer past the cap to notice the
	// overrun; anything near the 10 MiB sent means the cap did nothing.
	if n := body.read.Load(); n > limit+64<<10 {
		t.Errorf("read %d bytes of an undeclared body with a %d-byte cap", n, limit)
	}
}

// newAuthLimitServer is newLimitServer behind the authentication gate:
// one rule admits any verified caller to runbooks. It returns the
// issuer so a test can mint tokens the gate accepts.
func newAuthLimitServer(t *testing.T, cfg HostedConfig) (*HostedServer, *authntest.Issuer) {
	t.Helper()
	iss := authntest.NewIssuer(t)
	cfg.Auth = &authz.Config{
		Resource:  testResource,
		Providers: []authz.Provider{{Issuer: iss.URL, Audience: testAudience}},
		Rules:     []authz.Rule{{Collections: []string{"runbooks"}}},
	}
	cfg.HTTPClient = iss.Client()
	return newLimitServer(t, cfg), iss
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// The gate decides on headers alone, and an undeclared body is read only
// behind it: a caller the gate refuses has none of its body read, so
// unauthenticated work stays cheap. An admitted caller's body is still
// capped.
func TestHostedLimits_UndeclaredBodyIsReadOnlyAfterTheGateAdmits(t *testing.T) {
	const limit = 64 << 10
	srv, iss := newAuthLimitServer(t, HostedConfig{MaxRequestBytes: limit})
	good := iss.Token(t, authntest.Claims{Subject: "alice", Audience: testAudience})
	forged := iss.TokenSignedByOther(t, authntest.Claims{Subject: "mallory", Audience: testAudience})

	for _, tc := range []struct {
		name     string
		bearer   string
		want     int
		maxBytes int64
	}{
		{"no token", "", http.StatusUnauthorized, 0},
		{"forged token", forged, http.StatusUnauthorized, 0},
		// MaxBytesReader reads at most one buffer past the cap.
		{"admitted", good, http.StatusRequestEntityTooLarge, limit + 64<<10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &countingReader{left: 10 << 20}
			req := httptest.NewRequest(http.MethodPost, srv.EndpointPath(), body)
			req.ContentLength = -1 // chunked: the length is not declared
			req.Header.Set("Content-Type", "application/json")
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if n := body.read.Load(); n > tc.maxBytes {
				t.Errorf("read %d bytes of the body; want at most %d", n, tc.maxBytes)
			}
		})
	}
}

// A request whose undeclared body is still being read holds its
// in-flight slot: the cap counts every body buffer, so with one slot a
// second request is refused 503 until the first finishes.
func TestHostedLimits_UndeclaredBodyBeingReadHoldsItsSlot(t *testing.T) {
	srv, iss := newAuthLimitServer(t, HostedConfig{MaxConcurrentRequests: 1})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	url := ts.URL + srv.EndpointPath()
	good := iss.Token(t, authntest.Claims{Subject: "alice", Audience: testAudience})

	post := func(body io.Reader, bearer string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, url, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return ts.Client().Do(req)
	}
	status := func(t *testing.T, bearer string) int {
		t.Helper()
		resp, err := post(strings.NewReader(initializeBody), bearer)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// The first request sends half its body over a chunked stream and
	// then stalls with the stream open.
	pr, pw := io.Pipe()
	// Runs before ts.Close (cleanups are LIFO): a failing test must not
	// leave the server waiting on a body that never ends.
	t.Cleanup(func() { _ = pw.CloseWithError(errors.New("test ended")) })
	type result struct {
		code int
		err  error
	}
	first := make(chan result, 1)
	go func() {
		resp, err := post(pr, good)
		if err != nil {
			first <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		first <- result{code: resp.StatusCode}
	}()
	half := len(initializeBody) / 2
	if _, err := pw.Write([]byte(initializeBody[:half])); err != nil {
		t.Fatal(err)
	}

	// Until the first request is in its slot, an unauthenticated probe
	// is answered 401 by the gate; once it is, the probe is answered 503
	// before the gate runs.
	got := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if got = status(t, ""); got != http.StatusUnauthorized {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != http.StatusServiceUnavailable {
		t.Fatalf("second request while a chunked body is held open: status = %d, want 503", got)
	}
	if code := status(t, good); code != http.StatusServiceUnavailable {
		t.Errorf("authenticated second request: status = %d, want 503", code)
	}

	// Finishing the body completes the first request and frees the slot.
	if _, err := pw.Write([]byte(initializeBody[half:])); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	r := <-first
	if r.err != nil {
		t.Fatalf("first request: %v", r.err)
	}
	if r.code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", r.code)
	}
	if code := status(t, ""); code != http.StatusUnauthorized {
		t.Errorf("after the slot is freed: status = %d, want 401 from the gate", code)
	}
}

func TestHostedLimits_BodyUnderTheCapIsServed(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%v", chunked), func(t *testing.T) {
			srv := newLimitServer(t, HostedConfig{MaxRequestBytes: 4 << 10})
			req := httptest.NewRequest(http.MethodPost, srv.EndpointPath(), strings.NewReader(initializeBody))
			if chunked {
				req.ContentLength = -1
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"protocolVersion"`) {
				t.Errorf("initialize answer missing: %s", rec.Body.String())
			}
		})
	}
}

func TestHostedLimits_NegativeBodyCapIsRefused(t *testing.T) {
	_, err := NewHosted(context.Background(), HostedConfig{
		Collections:     threeCollectionRegistry(t),
		MaxRequestBytes: -1,
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be turned off") {
		t.Fatalf("err = %v, want the negative cap refused", err)
	}
}

func TestHostedLimits_DefaultsApply(t *testing.T) {
	var c HostedConfig
	c.applyDefaults()
	if c.MaxRequestBytes != DefaultMaxRequestBytes || c.MaxConcurrentRequests != DefaultMaxConcurrentRequests ||
		c.MaxConcurrentStreams != DefaultMaxConcurrentStreams {
		t.Errorf("defaults = %d/%d/%d", c.MaxRequestBytes, c.MaxConcurrentRequests, c.MaxConcurrentStreams)
	}
	// The largest legitimate call is a 256 KiB memory save, JSON-escaped.
	if DefaultMaxRequestBytes < 2<<20 {
		t.Errorf("DefaultMaxRequestBytes %d leaves no room for a maximal memory save", DefaultMaxRequestBytes)
	}
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("DefaultAddr %q does not bind loopback", c.Addr)
	}
}

func TestInflight_FullPoolIs503AndPoolsAreSeparate(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	block := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	})
	h := newInflight(1, 1).middleware(block)

	var wg sync.WaitGroup
	for _, m := range []string{http.MethodPost, http.MethodGet} {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(m, "/mcp", nil))
		}(m)
	}
	// One request and one stream are in: each pool has its own slot, so
	// an open stream does not take a request's.
	<-entered
	<-entered

	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodGet} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/mcp", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s over the cap: status = %d, want 503", m, rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s over the cap: no Retry-After", m)
		}
	}
	close(release)
	wg.Wait()

	// Slots are returned when a request ends.
	rec := httptest.NewRecorder()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	l := newInflight(1, 1)
	for range 3 {
		l.middleware(ok).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("sequential requests: status = %d, want 204 (a slot leaked)", rec.Code)
	}
}

func TestInflight_NegativeMeansUnlimited(t *testing.T) {
	l := newInflight(-1, -1)
	if l.requests != nil || l.streams != nil {
		t.Fatal("a negative cap built a pool")
	}
	rec := httptest.NewRecorder()
	l.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// --- metrics label discipline (meerkat-mob#47) ---------------------------

func TestHostedMetrics_MethodLabelIsAClosedSet(t *testing.T) {
	srv := newLimitServer(t, HostedConfig{})
	h := srv.Handler()
	rng := rand.New(rand.NewPCG(47, 47))
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for i := range 300 {
		b := make([]byte, 3+rng.IntN(8))
		for j := range b {
			b[j] = letters[rng.IntN(len(letters))]
		}
		method := fmt.Sprintf("%s%d", b, i)
		for _, path := range []string{srv.EndpointPath(), LivenessPath, "/nope"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Method = method
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	}
	// A lower-case "get" is not GET: methods are case-sensitive.
	req := httptest.NewRequest(http.MethodGet, LivenessPath, nil)
	req.Method = "get"
	h.ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetricsPath, nil))
	body := rec.Body.String()

	allowed := map[string]bool{"GET": true, "POST": true, "DELETE": true, "HEAD": true,
		"OPTIONS": true, "PUT": true, "PATCH": true, "other": true}
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "meerkat_http_request") {
			continue
		}
		_, rest, ok := strings.Cut(line, `method="`)
		if !ok {
			continue
		}
		m, _, _ := strings.Cut(rest, `"`)
		seen[m] = true
		if !allowed[m] {
			t.Errorf("method label %q is outside the closed set", m)
		}
	}
	if !seen["other"] {
		t.Errorf(`no method="other" series after 300 unknown methods:\n%s`, body)
	}
	if n := strings.Count(body, "meerkat_http_requests_total{"); n > 3*len(allowed)*4 {
		t.Errorf("%d request series: the label set is not closed", n)
	}
}

func TestMethodLabel(t *testing.T) {
	for in, want := range map[string]string{
		"GET": "GET", "POST": "POST", "DELETE": "DELETE", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
		"PUT": "PUT", "PATCH": "PATCH", "CONNECT": "other", "TRACE": "other", "get": "other",
		"ZZ0": "other", "": "other",
	} {
		if got := methodLabel(in); got != want {
			t.Errorf("methodLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- separate metrics listener -------------------------------------------

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestHostedMetrics_SeparateListenerMovesMetricsOffTheAPIPort(t *testing.T) {
	apiAddr, metricsAddr := freeAddr(t), freeAddr(t)
	srv := newLimitServer(t, HostedConfig{Addr: apiAddr, MetricsAddr: metricsAddr})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("ListenAndServe: %v", err)
		}
	}()

	get := func(addr, path string) (int, string) {
		t.Helper()
		var resp *http.Response
		var err error
		for range 50 {
			resp, err = http.Get("http://" + addr + path)
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET %s%s: %v", addr, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := get(apiAddr, LivenessPath); code != http.StatusOK {
		t.Fatalf("API livez = %d", code)
	}
	if code, _ := get(apiAddr, MetricsPath); code != http.StatusNotFound {
		t.Errorf("API port /metrics = %d, want 404 with a separate metrics listener", code)
	}
	code, body := get(metricsAddr, MetricsPath)
	if code != http.StatusOK || !strings.Contains(body, "meerkat_build_info") {
		t.Errorf("metrics listener /metrics = %d:\n%s", code, body)
	}
	if code, _ := get(metricsAddr, LivenessPath); code != http.StatusNotFound {
		t.Errorf("metrics listener serves %s (%d); it should serve /metrics only", LivenessPath, code)
	}
	_, banner := get(apiAddr, "/")
	if strings.Contains(banner, MetricsPath) {
		t.Errorf("the API banner advertises %s it does not serve:\n%s", MetricsPath, banner)
	}
}

func TestHostedMetrics_MetricsStayOnTheAPIPortByDefault(t *testing.T) {
	srv := newLimitServer(t, HostedConfig{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetricsPath, nil))
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("meerkat_build_info")) {
		t.Errorf("/metrics = %d", rec.Code)
	}
}
