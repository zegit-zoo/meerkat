package traversal

import (
	"strings"
	"testing"
)

// free_text_test.go pins meerkat-mob#53: without `query: plaintext` no
// caller-written text and no subject-derived namespace reaches the log.

const (
	ftSummary = "Rotate the payroll key in the console"
	ftSource  = "https://internal.example/doc?token=abc123"
	ftNotes   = "asked about the payroll migration"
	ftIntake  = "raw/alice-0123456789abcdef/2026-10-07/0011223344556677/page.md"
	ftNoNS    = "raw/2026-10-07/0011223344556677/page.md"
)

func freeTextEntry() Entry {
	return Entry{
		Session: "s", Outcome: "gave_up", InitialQuery: "payroll key",
		Quality:  &Quality{Accuracy: 0.5, Notes: ftNotes},
		Fallback: &Fallback{Kind: "web", Summary: ftSummary, Sources: []string{ftSource}},
		IntakeID: ftIntake,
	}
}

func TestRecord_DropsCallerTextByDefault(t *testing.T) {
	l, dir := openTestLog(t, Config{})
	e := freeTextEntry()
	text := recordedText(t, l, dir, e)
	for _, leak := range []string{ftSummary, "internal.example", "token=", ftNotes, "alice", "0123456789abcdef", ftNoNS, "payroll"} {
		if strings.Contains(text, leak) {
			t.Errorf("default log stored %q: %s", leak, text)
		}
	}
	for _, want := range []string{`"kind":"web"`, `"accuracy":0.5`, l.Hash(ftSource), `"intake_id":"` + l.Hash(ftNoNS) + `"`} {
		if !strings.Contains(text, want) {
			t.Errorf("default log lacks %q: %s", want, text)
		}
	}
	// The caller's values are untouched: Record works on copies.
	if e.Fallback.Summary != ftSummary || e.Fallback.Sources[0] != ftSource || e.Quality.Notes != ftNotes {
		t.Errorf("Record edited the caller's entry: %+v %+v", e.Fallback, e.Quality)
	}
}

func TestRecord_KeepsCallerTextWhenOptedIn(t *testing.T) {
	l, dir := openTestLog(t, Config{Query: QueryPlaintext})
	text := recordedText(t, l, dir, freeTextEntry())
	for _, want := range []string{ftSummary, ftNotes, `"intake_id":"` + ftNoNS + `"`, "internal.example"} {
		if !strings.Contains(text, want) {
			t.Errorf("opted-in log lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "alice") {
		t.Errorf("the intake namespace reached an opted-in log: %s", text)
	}
}

func TestStripIntakeNamespace(t *testing.T) {
	for in, want := range map[string]string{
		ftIntake:           ftNoNS,
		"":                 "",
		"raw/x/page.md":    "raw/x/page.md",
		"staged/kb/id.md":  "staged/kb/id.md",
		"promote:the-coll": "promote:the-coll",
	} {
		if got := stripIntakeNamespace(in); got != want {
			t.Errorf("stripIntakeNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}
