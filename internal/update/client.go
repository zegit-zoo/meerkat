package update

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Network timeouts. There is deliberately no http.Client.Timeout: that
// bounds the whole exchange including the body, which fails legitimate
// downloads on slow links. Instead the connect and response-header
// phases are bounded here, and the body read is bounded by an idle
// (stall) timeout plus the caller's context (see idleTimeoutReader).
const (
	connectTimeout        = 10 * time.Second
	responseHeaderTimeout = 20 * time.Second
)

// bodyIdleTimeout is a var so tests can shorten it.
var bodyIdleTimeout = 30 * time.Second

func newUpdateTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = connectTimeout
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}

var updateHTTPClient = &http.Client{
	Transport: newUpdateTransport(),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		// Go's http.Client only strips the Authorization header on a
		// redirect to a different HOST; it does not strip it on a
		// same-host scheme downgrade (https -> http). Since we send a
		// bearer token, an on-path attacker who can force/observe an
		// https->http redirect to an otherwise-allowlisted host would
		// otherwise receive the token in cleartext. Require https
		// regardless of host.
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("refusing redirect to non-https URL %q", req.URL.Redacted())
		}
		host := strings.ToLower(req.URL.Hostname())
		// Allow github.com API and its CDN asset hosts. GitHub asset
		// downloads redirect from api.github.com to
		// objects.githubusercontent.com (and similar *.githubusercontent.com
		// or *.github.com CDN hosts).
		if host == "github.com" ||
			strings.HasSuffix(host, ".github.com") ||
			strings.HasSuffix(host, ".githubusercontent.com") {
			return nil
		}
		return fmt.Errorf("refusing redirect to untrusted host %q", req.URL.Host)
	},
}

// idleTimeoutReader cancels the request (via cancel) when no bytes
// arrive for d, so a stalled connection fails fast while a slow but
// steady download is never cut off.
type idleTimeoutReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func newIdleTimeoutReader(r io.Reader, d time.Duration, cancel context.CancelFunc) *idleTimeoutReader {
	return &idleTimeoutReader{r: r, t: time.AfterFunc(d, cancel), d: d}
}

func (i *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.t.Reset(i.d)
	}
	return n, err
}

func (i *idleTimeoutReader) stop() { i.t.Stop() }

// assetURLPrefix is the only URL prefix a gh token may be sent to: the
// GitHub API's release-asset endpoint for this project. Asset URLs come
// out of a JSON response, so they are checked before any credential is
// attached; anything else is fetched anonymously.
func assetURLPrefix() string {
	return githubAPIBase + "/repos/" +
		path.Join(url.PathEscape(projectOwner()), url.PathEscape(projectRepo())) +
		"/releases/assets/"
}

// tokenAllowedForAsset reports whether a token may accompany a request
// for assetURL.
func tokenAllowedForAsset(assetURL string) bool {
	return strings.HasPrefix(assetURL, assetURLPrefix())
}

// isRateLimited reports whether resp is GitHub's rate-limit refusal
// (as opposed to an authorization failure). It may consume a little of
// the body.
func isRateLimited(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return true
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return strings.Contains(strings.ToLower(string(b)), "rate limit")
	}
	return false
}

// getAnonymousFirst issues the request anonymously and, only when GitHub
// answers with a rate-limit refusal and tokenOK is true, retries once
// with the bearer token. build must return a fresh request each call.
// The public repository needs no credential, so the token is never sent
// unless the anonymous quota is exhausted.
func getAnonymousFirst(build func() (*http.Request, error), token string, tokenOK bool) (*http.Response, error) {
	req, err := build()
	if err != nil {
		return nil, err
	}
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if token == "" || !tokenOK || !isRateLimited(resp) {
		return resp, nil
	}
	resp.Body.Close()
	req, err = build()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return updateHTTPClient.Do(req)
}
