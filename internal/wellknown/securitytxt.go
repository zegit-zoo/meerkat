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
	"time"
)

// SecurityTxtPath is where RFC 9116 §3 puts the file.
const SecurityTxtPath = "/.well-known/security.txt"

const (
	contactEmail = "mailto:security@primitive-engineering.se"
	advisoryURL  = "https://github.com/zegit-zoo/meerkat/security/advisories/new"
	policyURL    = "https://github.com/zegit-zoo/meerkat/blob/master/SECURITY.md"
	// validity is how far ahead Expires lies. RFC 9116 §2.5.5
	// recommends under a year. The file is generated per request, so it
	// is never stale, however long the process has run or however old
	// the binary is.
	validity = 180 * 24 * time.Hour
)

// SecurityTxt is the file's body as served at now.
func SecurityTxt(now time.Time) string {
	return fmt.Sprintf(`# Security contact for the meerkat software, not this deployment.
# meerkat is self-hosted: for this host or its configuration, ask its operator.
Contact: %s
Contact: %s
Expires: %s
Policy: %s
Preferred-Languages: en, sv
`, contactEmail, advisoryURL, now.UTC().Add(validity).Truncate(time.Second).Format(time.RFC3339), policyURL)
}

// SecurityTxtHandler serves SecurityTxt at the time of each request.
// It needs no authentication: its whole job is to be readable by
// someone who has no credentials.
func SecurityTxtHandler(now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, SecurityTxt(now()))
	})
}
