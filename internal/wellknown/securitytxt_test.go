package wellknown

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// securitytxt_test.go pins #126 (MK-A-3): the RFC 9116 security.txt
// that `mk http serve` and `mk mcp serve-http` publish.

// fields parses a security.txt body into its field lines, failing on
// anything that is neither a field nor a comment nor blank.
func fields(t *testing.T, body string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok || name == "" || value == "" {
			t.Fatalf("not an RFC 9116 field line: %q", line)
		}
		out[name] = append(out[name], value)
	}
	return out
}

func TestSecurityTxt_RequiredFieldsAndContacts(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := fields(t, SecurityTxt(now))
	contacts := strings.Join(f["Contact"], " ")
	for _, want := range []string{"mailto:security@primitive-engineering.se", "https://github.com/zegit-zoo/meerkat/security/advisories/new"} {
		if !strings.Contains(contacts, want) {
			t.Errorf("Contact lacks %q: %v", want, f["Contact"])
		}
	}
	if len(f["Expires"]) != 1 {
		t.Fatalf("Expires must appear exactly once (RFC 9116 §2.5.5): %v", f["Expires"])
	}
	if got := f["Policy"]; len(got) != 1 || got[0] != "https://github.com/zegit-zoo/meerkat/blob/master/SECURITY.md" {
		t.Errorf("Policy = %v", got)
	}
	if got := f["Preferred-Languages"]; len(got) != 1 || got[0] != "en, sv" {
		t.Errorf("Preferred-Languages = %v", got)
	}
}

// An unstamped build (go build, go install) has no build date, so Expires
// is computed from the time of the request and is under a year away as
// RFC 9116 recommends.
func TestSecurityTxt_UnstampedExpiresIsFreshAndUnderAYear(t *testing.T) {
	SetBuildDate("unknown")
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	exp, err := time.Parse(time.RFC3339, fields(t, SecurityTxt(now))["Expires"][0])
	if err != nil {
		t.Fatalf("Expires is not RFC 3339: %v", err)
	}
	if d := exp.Sub(now); d <= 30*24*time.Hour || d >= 365*24*time.Hour {
		t.Errorf("Expires is %v after now; want more than 30 days and under a year", d)
	}
	later := now.AddDate(3, 0, 0)
	exp2, _ := time.Parse(time.RFC3339, fields(t, SecurityTxt(later))["Expires"][0])
	if !exp2.After(later) {
		t.Errorf("a file served three years later expires at %v, before it is served", exp2)
	}
}

// A release binary carries its build date: Expires is fixed from it, the
// same on every request however long the process has run, still 30 days
// to a year after the build, and it does move forward with a new build.
func TestSecurityTxt_StampedExpiresIsFixedAtBuildTime(t *testing.T) {
	t.Cleanup(func() { SetBuildDate("unknown") })
	built := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	SetBuildDate(built.Format(time.RFC3339))

	want := built.Add(validity).Format(time.RFC3339)
	for _, now := range []time.Time{built, built.AddDate(0, 6, 0), built.AddDate(3, 0, 0)} {
		got := fields(t, SecurityTxt(now))["Expires"]
		if len(got) != 1 || got[0] != want {
			t.Fatalf("Expires served at %v = %v, want the fixed %s", now, got, want)
		}
	}
	exp, _ := time.Parse(time.RFC3339, want)
	if d := exp.Sub(built); d <= 30*24*time.Hour || d >= 365*24*time.Hour {
		t.Errorf("Expires is %v after the build; want more than 30 days and under a year", d)
	}

	SetBuildDate(built.AddDate(0, 3, 0).Format(time.RFC3339))
	if got := fields(t, SecurityTxt(built))["Expires"][0]; got == want {
		t.Error("a later build must advance Expires")
	}
}

// The file says whose policy it is: the software's, not the operator's
// deployment's.
func TestSecurityTxt_SaysItCoversTheSoftware(t *testing.T) {
	body := SecurityTxt(time.Now())
	if !strings.Contains(body, "# ") || !strings.Contains(strings.ToLower(body), "not this deployment") {
		t.Errorf("no comment scoping the file to the meerkat software:\n%s", body)
	}
}

func TestSecurityTxtHandler_ServesPlainText(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	rec := httptest.NewRecorder()
	SecurityTxtHandler(func() time.Time { return now }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SecurityTxtPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q (RFC 9116 §3)", ct)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if rec.Body.String() != SecurityTxt(now) {
		t.Error("the handler serves something other than SecurityTxt(now)")
	}
}
