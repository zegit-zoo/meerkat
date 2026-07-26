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
