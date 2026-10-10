package cli

import (
	"io"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// Terminal-safe output for the human-readable (non --json) forms of the
// commands that print knowledge-base content: mk search, show, list and
// lint.
//
// Page titles, bodies, hints, snippets, IDs and frontmatter come from
// whatever the mounted collections serve, which includes remote url, s3
// and gcs sources the operator does not author. A control character in
// that content is not text: ESC and the C1 introducers start terminal
// escape sequences that can move the cursor, rewrite lines already on
// screen, set the window title or, on some terminals, write the
// clipboard. Printing content verbatim lets a collection's author
// rewrite what the operator sees.
//
// So every byte those commands write to stdout in their text form passes
// through stripControl. --json output is untouched: encoding/json escapes
// C0 controls itself, and a JSON consumer needs the exact stored value.

// stripControl returns s with every C0 control character (U+0000–U+001F),
// DEL (U+007F) and C1 control character (U+0080–U+009F) removed, except
// '\n' and '\t', which page bodies legitimately contain. '\r' is removed
// too: on its own it returns the cursor to the start of the line, which
// is enough to overwrite what was printed before it.
//
// Bytes that are not valid UTF-8 are replaced with U+FFFD rather than
// passed through: a lone 0x9B byte is the 8-bit form of the C1 CSI
// introducer to a terminal that is not decoding UTF-8.
func stripControl(s string) string {
	if isTerminalSafe(s) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isStrippedControl(r) {
			return -1
		}
		return r
	}, s)
}

// isTerminalSafe is stripControl's fast path: true when s is valid UTF-8
// with nothing to strip, so the common case allocates nothing.
func isTerminalSafe(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || isStrippedControl(r) {
			return false
		}
	}
	return true
}

func isStrippedControl(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	}
	return false
}

// controlStripper is an io.Writer that writes everything through
// stripControl. Each fmt.Fprint* call is one Write of one fully formatted
// string, so stripping per Write sees whole runes.
type controlStripper struct{ w io.Writer }

func (c controlStripper) Write(p []byte) (int, error) {
	if _, err := io.WriteString(c.w, stripControl(string(p))); err != nil {
		return 0, err
	}
	// Report the caller's whole buffer as consumed: the stripped bytes
	// were handled, by being dropped, not left unwritten.
	return len(p), nil
}

// textOut is the stdout a command's human-readable output goes to: the
// command's own stdout, with control characters stripped.
func textOut(cmd *cobra.Command) io.Writer {
	return controlStripper{w: cmd.OutOrStdout()}
}
