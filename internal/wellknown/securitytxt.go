// Package wellknown serves meerkat's RFC 9116 security.txt (#126,
// MK-A-3; part of the compliance epic #68).
//
// meerkat is self-hosted, and a security.txt normally names the
// security contact of the site that serves it. This one names the
// contact for the meerkat software, and says so in a comment, because
// a flaw a researcher finds through a meerkat endpoint is most likely a
// flaw in meerkat. An operator who serves a security.txt of their own
// for the host turns this one off (`--security-txt=false`); operator
// decision, 2026-10-02.
package wellknown

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// SecurityTxtPath is where RFC 9116 §3 puts the file.
const SecurityTxtPath = "/.well-known/security.txt"

const (
	contactEmail = "mailto:security@primitive-engineering.se"
	advisoryURL  = "https://github.com/zegit-zoo/meerkat/security/advisories/new"
	policyURL    = "https://github.com/zegit-zoo/meerkat/blob/master/SECURITY.md"
	// validity is how far ahead of the build Expires lies. RFC 9116
	// §2.5.5 recommends under a year.
	validity = 180 * 24 * time.Hour
)

// buildTime is when this binary was built, as stamped by the release
// pipeline through the build-date linker flag (see SetBuildDate).
var buildTime atomic.Pointer[time.Time]

// SetBuildDate records the build timestamp (RFC 3339, the format the
// Makefile, the Dockerfile and goreleaser all stamp). Expires is then
// fixed at build time + 180 days, the same for every request and every
// restart of that binary, so a deployment that is never upgraded stops
// advertising a fresh contact and the file goes stale, which is the
// signal RFC 9116 intends. An unparseable value (an unstamped
// `go build`, "unknown") leaves the legacy behaviour: Expires is
// computed per request, now + 180 days.
//
// Operators who want a different Expires have two choices: serve their
// own file (--security-txt=false) or build with their own build-date
// stamp.
func SetBuildDate(s string) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		buildTime.Store(nil)
		return
	}
	buildTime.Store(&t)
}

// expires is the Expires value: fixed from the build date when one was
// stamped, otherwise relative to now.
func expires(now time.Time) time.Time {
	if bt := buildTime.Load(); bt != nil {
		return bt.UTC().Add(validity).Truncate(time.Second)
	}
	return now.UTC().Add(validity).Truncate(time.Second)
}

// SecurityTxt is the file's body as served at now.
func SecurityTxt(now time.Time) string {
	return fmt.Sprintf(`# Security contact for the meerkat software, not this deployment.
# meerkat is self-hosted: for this host or its configuration, ask its operator.
Contact: %s
Contact: %s
Expires: %s
Policy: %s
Preferred-Languages: en, sv
`, contactEmail, advisoryURL, expires(now).Format(time.RFC3339), policyURL)
}

// SecurityTxtHandler serves SecurityTxt at the time of each request.
// It needs no authentication: its whole job is to be readable by
// someone who has no credentials.
func SecurityTxtHandler(now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = fmt.Fprint(w, SecurityTxt(now()))
	})
}
