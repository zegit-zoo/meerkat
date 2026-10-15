package gitinfo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// repoconfig.go decides whether a repository's own git configuration is
// safe to run git inside (meerkat-mob#31).
//
// An in-repository call (`git status`, `git pull`) reads the repository's
// local config, and several keys there make git run a program or send the
// call somewhere else: an ssh or proxy command, a credential helper, a
// clean/smudge filter (which `.gitattributes` in the tree applies on status
// and checkout), a diff textconv, a merge driver, an included file that
// could carry any of these, a URL rewrite, a worktree elsewhere on disk.
// Command-line overrides cannot neutralise all of them — a filter driver
// has an arbitrary name, and some keys are first-match — so a repository
// that sets any of them is refused before git runs.
//
// The scanner here is strict where the reader in config.go is lenient: a
// line it cannot classify is a refusal, not "absent", so a key cannot hide
// from it behind syntax git accepts and this file does not understand.

// ErrUnsafeConfig is a repository whose own git config sets a key that
// could make an in-repository git call run a program or reach somewhere
// other than the configured remote. No git command is run in it.
var ErrUnsafeConfig = errors.New("refused: the repository's git config sets a key meerkat does not run git under")

// deniedSections are refused whatever the key: every key in them either
// runs a program, pulls in more configuration, or rewrites where git goes.
var deniedSections = map[string]bool{
	"credential": true, // helpers run programs; credential.<url>.* too
	"filter":     true, // clean / smudge / process drivers
	"include":    true, // include.path
	"includeif":  true, // includeIf.<cond>.path
	"url":        true, // insteadOf / pushInsteadOf rewrites
	"gpg":        true, // gpg.program, gpg.<format>.program
}

// deniedKeys are refused by section and key, whatever the subsection.
var deniedKeys = map[string]map[string]bool{
	"core": {
		"sshcommand":           true,
		"gitproxy":             true,
		"worktree":             true, // the fast-forward would write elsewhere
		"askpass":              true,
		"alternaterefscommand": true,
	},
	"diff":   {"external": true, "textconv": true, "command": true},
	"merge":  {"driver": true},
	"remote": {"vcs": true, "uploadpack": true},
}

func deniedConfigKey(section, key string) bool {
	return deniedSections[section] || deniedKeys[section][key]
}

// CheckRepoConfig refuses (ErrUnsafeConfig) a working tree whose own git
// configuration — the common `config` and a linked worktree's
// `config.worktree` — sets a denied key or cannot be read strictly. It
// reads files only. root is the working tree (or a directory inside it).
func CheckRepoConfig(root string) error {
	repo, err := Find(root)
	if err != nil {
		return err
	}
	files := []string{filepath.Join(repo.commonDir, "config")}
	files = append(files, filepath.Join(repo.gitDir, "config.worktree"))
	if repo.commonDir != repo.gitDir {
		files = append(files, filepath.Join(repo.commonDir, "config.worktree"))
	}
	for _, f := range files {
		b, err := readCapped(f, maxConfigFile)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := checkConfigText(string(b)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

var (
	// [section] or [section "subsection"]; a legacy [section.sub] has dots
	// in the name. The remainder after `]` is returned for a same-line key.
	headerRE = regexp.MustCompile(`^\[\s*([A-Za-z0-9.-]+)\s*(?:"((?:[^"\\]|\\.)*)")?\s*\](.*)$`)
	keyRE    = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*)[ \t]*(.*)$`)
)

// checkConfigText refuses text that sets a denied key, and text it cannot
// classify line by line. It follows git's own line rules where they decide
// what is a key: a value continues onto the next line only when it ends in
// an unescaped backslash outside a comment (inside or outside quotes), and
// a key may follow its section header on the same line.
func checkConfigText(text string) error {
	text = strings.TrimPrefix(text, "\ufeff")
	if strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: a NUL byte", ErrUnsafeConfig)
	}
	lines := strings.Split(text, "\n")
	section := ""
	for i := 0; i < len(lines); i++ {
		// Only leading blanks are dropped: the value's trailing bytes
		// decide whether it continues (`\ ` escapes a space, `\` at the
		// very end joins the next line).
		line := strings.TrimLeft(strings.TrimSuffix(lines[i], "\r"), " \t")
		if blankOrComment(line) {
			continue
		}
		if line[0] == '[' {
			m := headerRE.FindStringSubmatch(line)
			if m == nil {
				return fmt.Errorf("%w: an unreadable section header", ErrUnsafeConfig)
			}
			section, _, _ = strings.Cut(strings.ToLower(m[1]), ".")
			if section == "" {
				return fmt.Errorf("%w: an unreadable section header", ErrUnsafeConfig)
			}
			// git accepts a key on the same line as its header.
			line = strings.TrimLeft(m[3], " \t")
			if blankOrComment(line) {
				continue
			}
		}
		m := keyRE.FindStringSubmatch(line)
		if m == nil || section == "" {
			return fmt.Errorf("%w: an unreadable line", ErrUnsafeConfig)
		}
		rest := m[2]
		if !blankOrComment(rest) && rest[0] != '=' {
			return fmt.Errorf("%w: an unreadable line", ErrUnsafeConfig)
		}
		if key := strings.ToLower(m[1]); deniedConfigKey(section, key) {
			return fmt.Errorf("%w: %s.%s", ErrUnsafeConfig, section, key)
		}
		if rest == "" || rest[0] != '=' {
			continue // a bare boolean key, or a key and a comment
		}
		// Skip the value's continuation lines: they are value, not keys.
		inQuote, cont := scanValue(false, rest[1:])
		for cont && i+1 < len(lines) {
			i++
			inQuote, cont = scanValue(inQuote, strings.TrimSuffix(lines[i], "\r"))
		}
	}
	return nil
}

// scanValue reads one physical line of a config value the way git does,
// starting inside a quote or not, and reports the quote state at its end
// and whether the value continues onto the next line (a final, unescaped
// backslash that is not inside a comment).
func scanValue(inQuote bool, s string) (quote, continues bool) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			if i == len(s)-1 {
				return inQuote, true
			}
			i++ // an escaped character, whatever it is
		case c == '"':
			inQuote = !inQuote
		case (c == ';' || c == '#') && !inQuote:
			return inQuote, false // a comment runs to the end of the line
		}
	}
	return inQuote, false
}

// blankOrComment is a line (or the rest of one) that carries no key.
func blankOrComment(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || s[0] == '#' || s[0] == ';'
}
