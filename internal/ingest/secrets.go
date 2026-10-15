package ingest

import "regexp"

// secrets.go is a small built-in secret scan (meerkat-mob#34): a
// researcher candidate, and the deposit it is drafted from, is refused
// when it carries something shaped like a credential. The agent works
// on the operator's machine, so a page that quotes a key is far more
// likely a leak than knowledge. The patterns follow the shapes gitleaks'
// default rules look for; they are a tripwire, not a scanner, and the
// repository's gitleaks run stays the gate for the content repo itself.

type secretRule struct {
	id string
	re *regexp.Regexp
}

var secretRules = []secretRule{
	{"private-key", regexp.MustCompile(`-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----`)},
	{"aws-access-key-id", regexp.MustCompile(`\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{"aws-secret-access-key", regexp.MustCompile(`(?i)aws_?secret_?access_?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}\b`)},
	{"github-token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})\b`)},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{24,}=*`)},
}

// findSecret returns the id of the first rule b matches, or "".
func findSecret(b []byte) string {
	for _, r := range secretRules {
		if r.re.Match(b) {
			return r.id
		}
	}
	return ""
}
