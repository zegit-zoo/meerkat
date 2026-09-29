package gitinfo

import (
	"path/filepath"
	"strings"
)

// gitConfig is the part of a git config file this package needs: the
// last value of each `section.subsection.key`. Section and key names are
// case-insensitive in git and are lowercased here; a subsection is
// case-sensitive and kept as written.
//
// It is a reader for the common shapes git itself writes (`[branch
// "main"]`, `[remote "origin"]`, `key = value`, quoted values, comments),
// not a full implementation. It follows no `[include]`, applies no
// `insteadOf`, and treats anything it cannot parse as absent, so a
// surprise reads as "no upstream" and never as a wrong one.
type gitConfig map[string]string

func (c gitConfig) get(section, subsection, key string) string {
	return c[strings.ToLower(section)+"\x00"+subsection+"\x00"+strings.ToLower(key)]
}

func (r *Repo) config() (gitConfig, error) {
	b, err := readCapped(filepath.Join(r.commonDir, "config"), maxConfigFile)
	if err != nil {
		return nil, err
	}
	return parseConfig(string(b)), nil
}

func parseConfig(text string) gitConfig {
	cfg := gitConfig{}
	var section, subsection string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			section, subsection = parseSectionHeader(line)
			continue
		}
		if section == "" {
			continue
		}
		key, value, hasValue := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t\"") {
			continue
		}
		v := "true" // a bare key is a boolean true in git
		if hasValue {
			v = parseValue(value)
		}
		cfg[section+"\x00"+subsection+"\x00"+strings.ToLower(key)] = v
	}
	return cfg
}

// parseSectionHeader reads `[section]`, `[section "sub"]` and the legacy
// `[section.sub]` (whose subsection git lowercases).
func parseSectionHeader(line string) (section, subsection string) {
	end := strings.LastIndexByte(line, ']')
	if end < 0 {
		return "", ""
	}
	inner := strings.TrimSpace(line[1:end])
	if name, rest, ok := strings.Cut(inner, " "); ok {
		rest = strings.TrimSpace(rest)
		if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
			sub := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(rest[1 : len(rest)-1])
			return strings.ToLower(name), sub
		}
		return "", ""
	}
	if name, sub, ok := strings.Cut(inner, "."); ok {
		return strings.ToLower(name), strings.ToLower(sub)
	}
	return strings.ToLower(inner), ""
}

// parseValue unquotes a value and drops a trailing comment outside
// quotes. Backslash escapes \" \\ \n \t are honoured.
func parseValue(v string) string {
	var b strings.Builder
	inQuote := false
	v = strings.TrimSpace(v)
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\' && i+1 < len(v):
			i++
			switch v[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(v[i])
			}
		case c == '"':
			inQuote = !inQuote
		case (c == '#' || c == ';') && !inQuote:
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}
