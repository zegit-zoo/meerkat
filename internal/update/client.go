package update

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

var updateHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
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
