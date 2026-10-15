package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

func TestStripControl(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text untouched", "Rate limiting — överblick", "Rate limiting — överblick"},
		{"newline and tab kept", "a\n\tb\n", "a\n\tb\n"},
		{"CSI sequence loses its ESC", "red\x1b[31mtext", "red[31mtext"},
		{"OSC title sequence", "\x1b]0;title\x07after", "]0;titleafter"},
		{"carriage return", "visible\rhidden", "visiblehidden"},
		{"NUL, backspace, DEL", "a\x00b\x08c\x7fd", "abcd"},
		{"C1 CSI as a rune", "x\u009b31my", "x31my"},
		{"C1 range edges", "\u0080a\u009fb\u00a0c", "ab\u00a0c"},
		{"lone 0x9b byte becomes U+FFFD", "x\x9b31my", "x\ufffd31my"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripControl(tc.in); got != tc.want {
				t.Errorf("stripControl(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripControl_CleanInputIsNotCopied(t *testing.T) {
	s := strings.Repeat("plain page body\n\t", 64)
	if n := testing.AllocsPerRun(100, func() { _ = stripControl(s) }); n != 0 {
		t.Errorf("stripControl allocated %v times on clean input, want 0", n)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestControlStripper_ReportsWholeBufferAndErrors(t *testing.T) {
	var buf bytes.Buffer
	n, err := controlStripper{w: &buf}.Write([]byte("a\x1bb"))
	if err != nil || n != 3 || buf.String() != "ab" {
		t.Errorf("Write = %d, %v, %q; want 3, nil, %q", n, err, buf.String(), "ab")
	}
	if _, err := (controlStripper{w: failingWriter{}}).Write([]byte("x")); err == nil {
		t.Error("a failing underlying writer must surface its error")
	}
}

// hostilePages carries control characters in every KB-derived field the
// text output of search, show, list and lint prints.
func hostilePages() []kb.Page {
	const esc = "\x1b]0;owned\x07\x1b[2J\r"
	return []kb.Page{
		{
			ID:    "concepts/esc" + "\x1b[1m",
			Title: "Escape" + esc + " title",
			Body:  "zebrafish body line\n\tindented" + esc + "\u009b31m",
			Front: kb.Frontmatter{
				Status:        "ok" + esc,
				FailureReason: "failed" + esc,
				Related:       []string{"missing" + esc},
			},
		},
		{
			ID:    "pointers/esc",
			Title: "Pointer" + esc,
			Body:  "zebrafish pointer body",
			Front: kb.Frontmatter{
				Type:   kb.TypePointer,
				Target: "collection:nope" + esc,
				Hint:   "zebrafish hint" + esc,
			},
		},
	}
}

// assertTerminalSafe fails when out contains any character
// stripControl would have removed.
func assertTerminalSafe(t *testing.T, what, out string) {
	t.Helper()
	if out == "" {
		t.Fatalf("%s: no output", what)
	}
	if got := stripControl(out); got != out {
		t.Errorf("%s: output carries control characters: %q", what, out)
	}
}

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	_ = cmd.Execute() // lint exits non-zero on the dangling links; the output is what is under test
	return out.String()
}

// TestTextOutput_StripsControlCharacters proves the human-readable
// output of every command that prints KB content is terminal-safe, and
// that the text around the stripped characters, including newlines and
// tabs, survives.
func TestTextOutput_StripsControlCharacters(t *testing.T) {
	withLintRegistry(t, hostilePages())

	search := runCmd(t, newSearchCmd(), "zebrafish", "--body")
	assertTerminalSafe(t, "mk search", search)
	for _, want := range []string{"Escape]0;owned[2J title", "zebrafish hint", "line\n\tindented"} {
		if !strings.Contains(search, want) {
			t.Errorf("mk search output lost %q:\n%s", want, search)
		}
	}

	show := runCmd(t, newShowCmd(), "concepts/esc\x1b[1m")
	assertTerminalSafe(t, "mk show", show)
	if !strings.Contains(show, "zebrafish body line\n\tindented") {
		t.Errorf("mk show output lost the body:\n%s", show)
	}
	assertTerminalSafe(t, "mk show (pointer)", runCmd(t, newShowCmd(), "pointers/esc"))

	list := runCmd(t, newListCmd())
	assertTerminalSafe(t, "mk list", list)
	if !strings.Contains(list, "failed]0;owned[2J") {
		t.Errorf("mk list output lost the failure reason:\n%s", list)
	}
	assertTerminalSafe(t, "mk list --collections", runCmd(t, newListCmd(), "--collections"))

	assertTerminalSafe(t, "mk lint", runCmd(t, newLintCmd()))
}

// TestJSONOutput_KeepsExactValues proves --json is untouched: the
// stored value round-trips byte for byte, escaped by encoding/json.
func TestJSONOutput_KeepsExactValues(t *testing.T) {
	pages := hostilePages()
	withLintRegistry(t, pages)

	var show struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	out := runCmd(t, newShowCmd(), "concepts/esc\x1b[1m", "--json")
	if err := json.Unmarshal([]byte(out), &show); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if show.Title != pages[0].Title || show.Body != pages[0].Body {
		t.Errorf("--json changed the stored values: %q / %q", show.Title, show.Body)
	}
}
