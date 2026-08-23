package contentsource

import (
	"context"
	"strings"
	"testing"
)

// update_test.go covers the per-collection update contract: that
// `description:` and the three `update:` methods parse with their
// documented defaults, that a contradictory contract (method: direct on
// a backend nothing can be written into) fails at LOAD time rather than
// at the first write, and that the contract survives resolution into the
// resolved collection a registry is built from.

func TestParseConfig_UpdateContract(t *testing.T) {
	cfg, err := parseConfig([]byte(`
collections:
  - name: handbook
    type: gcs
    bucket: my-org-knowledge
    prefix: handbook/live/
    description: Engineering handbook — conventions, onboarding, how we work.
    update:
      method: merge-request
      repo: https://github.com/example-org/handbook.git
      host: github
      path: wiki
      instructions: |
        Fork, branch, open a PR.
        One page per PR.
  - name: scratch
    type: local
    path: ../scratch
    update:
      method: direct
  - name: vendor
    type: url
    url: https://example.com/kb/vendor-v2.tar.gz
    sha256: `+strings.Repeat("a", 64)+`
    update:
      method: none
  - name: legacy
    type: local
    path: ../legacy
`), "content-source.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	handbook := cfg.Collections[0]
	if handbook.Description != "Engineering handbook — conventions, onboarding, how we work." {
		t.Errorf("description = %q", handbook.Description)
	}
	u := handbook.Update
	if u == nil || u.Method != UpdateMergeRequest {
		t.Fatalf("handbook update = %+v", u)
	}
	if u.Repo != "https://github.com/example-org/handbook.git" || u.Host != UpdateHostGitHub || u.Path != "wiki" {
		t.Errorf("handbook update = %+v", u)
	}
	// branch: is optional and defaults, so every rendered contract names
	// a branch.
	if u.Branch != DefaultUpdateBranch {
		t.Errorf("branch = %q, want the %q default", u.Branch, DefaultUpdateBranch)
	}
	if !strings.Contains(u.Instructions, "One page per PR.") {
		t.Errorf("instructions did not survive: %q", u.Instructions)
	}

	if got := cfg.Collections[1].Update.DeclaredMethod(); got != UpdateDirect {
		t.Errorf("scratch method = %q", got)
	}
	if got := cfg.Collections[2].Update.DeclaredMethod(); got != UpdateNone {
		t.Errorf("vendor method = %q", got)
	}
	// No update: block at all means no contribution path — which is what
	// every configuration written before this feature has.
	legacy := cfg.Collections[3]
	if legacy.Update != nil {
		t.Errorf("legacy picked up a contract it never declared: %+v", legacy.Update)
	}
	if got := legacy.Update.DeclaredMethod(); got != UpdateNone {
		t.Errorf("an absent update: block should read as %q, got %q", UpdateNone, got)
	}
	if legacy.Description != "" {
		t.Errorf("legacy description = %q, want empty", legacy.Description)
	}
}

func TestParseConfig_UpdateContractOnASingleSource(t *testing.T) {
	cfg, err := parseConfig([]byte(`
content:
  type: local
  path: kb
  description: The one knowledge base this deployment serves.
  update:
    method: merge-request
    repo: git@gitlab.example.com:platform/kb.git
    host: gitlab
    branch: trunk
`), "content-source.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Content.Description == "" {
		t.Error("content description was dropped")
	}
	u := cfg.Content.Update
	if u == nil || u.Method != UpdateMergeRequest || u.Host != UpdateHostGitLab || u.Branch != "trunk" {
		t.Fatalf("content update = %+v", u)
	}
	if u.Repo != "git@gitlab.example.com:platform/kb.git" {
		t.Errorf("an scp-like ssh remote should be accepted verbatim, got %q", u.Repo)
	}
}

func TestParseConfig_UpdateNormalizesAndDefaults(t *testing.T) {
	cfg, err := parseConfig([]byte(`
content:
  type: local
  path: kb
  update:
    method: "  Merge-Request  "
    repo: https://forge.example.com/team/kb.git
    path: "pages/"
    instructions: "  keep it short\n"
`), "content-source.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	u := cfg.Content.Update
	if u.Method != UpdateMergeRequest {
		t.Errorf("method = %q, want it trimmed and lowercased", u.Method)
	}
	// An unnamed host is "other": meerkat has no opinion about the forge,
	// so an agent follows instructions: and plain git.
	if u.Host != UpdateHostOther {
		t.Errorf("host = %q, want the %q default", u.Host, UpdateHostOther)
	}
	if u.Branch != DefaultUpdateBranch {
		t.Errorf("branch = %q, want the %q default", u.Branch, DefaultUpdateBranch)
	}
	if u.Path != "pages" {
		t.Errorf("path = %q, want the trailing slash trimmed", u.Path)
	}
	if u.Instructions != "keep it short" {
		t.Errorf("instructions = %q, want trimmed", u.Instructions)
	}
}

// TestParseConfig_UpdateDirectNeedsAWritableBackend is the acceptance
// criterion: a contract that promises a direct write must be declared on
// a backend a write can actually land in. Everything else is a snapshot
// — the write would go nowhere, or into a cache entry the next content
// resolution replaces — and that is data loss discovered days later by
// whoever wrote it.
func TestParseConfig_UpdateDirectNeedsAWritableBackend(t *testing.T) {
	direct := "\n    update:\n      method: direct\n"
	for name, tc := range map[string]struct {
		body    string
		wantErr string // "" means the config must be accepted
	}{
		"local directory": {
			"collections:\n  - name: c\n    type: local\n    path: ../kb" + direct,
			"",
		},
		"gcs prefix": {
			"collections:\n  - name: c\n    type: gcs\n    bucket: b\n    prefix: kb/live/" + direct,
			"",
		},
		"url tarball": {
			"collections:\n  - name: c\n    type: url\n    url: https://example.com/kb.tar.gz\n    sha256: " + strings.Repeat("a", 64) + direct,
			"type: url",
		},
		"gcs object tarball": {
			"collections:\n  - name: c\n    type: gcs\n    bucket: b\n    object: bundles/kb.tar.gz" + direct,
			"type: gcs with object:",
		},
		"embedded": {
			"content:\n  type: none\n  update:\n    method: direct\n",
			"type: none",
		},
		"build-time git source": {
			"collections:\n  - name: c\n    type: git\n    repo: example-org/kb\n    ref: v1.0.0" + direct,
			"build-time only",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.body), "content-source.yaml")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("a writable backend must accept method: direct: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseConfig accepted method: direct on a read-only backend, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to name the backend (%q)", err, tc.wantErr)
			}
			// The rejection has to be actionable: it must say what to do
			// instead, or an operator just deletes the block.
			if !strings.Contains(err.Error(), UpdateMergeRequest) {
				t.Errorf("the rejection should point at method: %s, got %v", UpdateMergeRequest, err)
			}
		})
	}
}

// TestParseConfig_MergeRequestIsLegalOnEveryBackend pins the other half
// of "declared, not inferred": the serving mirror and the contribution
// repo are different addresses, so a review flow is legal even on a
// backend meerkat could never write to.
func TestParseConfig_MergeRequestIsLegalOnEveryBackend(t *testing.T) {
	mr := "\n    update:\n      method: merge-request\n      repo: https://github.com/example-org/kb.git\n      host: github\n"
	for name, body := range map[string]string{
		"url tarball":        "collections:\n  - name: c\n    type: url\n    url: https://example.com/kb.tar.gz\n    sha256: " + strings.Repeat("a", 64) + mr,
		"gcs object tarball": "collections:\n  - name: c\n    type: gcs\n    bucket: b\n    object: bundles/kb.tar.gz" + mr,
		"gcs prefix":         "collections:\n  - name: c\n    type: gcs\n    bucket: b\n    prefix: kb/live/" + mr,
		"local directory":    "collections:\n  - name: c\n    type: local\n    path: ../kb" + mr,
		"embedded":           "content:\n  type: none\n  update:\n    method: merge-request\n    repo: https://github.com/example-org/kb.git\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(body), "content-source.yaml"); err != nil {
				t.Fatalf("method: merge-request must be legal on any backend: %v", err)
			}
		})
	}
}

func TestParseConfig_UpdateValidationRunsAtLoadTime(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no method": {
			"content:\n  type: local\n  path: kb\n  update:\n    repo: https://github.com/example-org/kb.git\n",
			"update.method is required",
		},
		"unknown method": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: pull-request\n",
			`update.method must be one of direct|merge-request|none, got "pull-request"`,
		},
		"merge-request with no repo": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: merge-request\n",
			"update.repo is required for method: merge-request",
		},
		"repo slug names no host": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: merge-request\n    repo: example-org/kb\n",
			"must be an https:// URL, an ssh:// URL, or the git@host:owner/repo form",
		},
		"repo over plain http": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: merge-request\n    repo: http://forge.example.com/team/kb.git\n",
			"must be an https:// URL",
		},
		// SECURITY: a contract is rendered to every caller who can see
		// the collection, so a token in the repo URL is a token handed to
		// them.
		"repo with an embedded token": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: merge-request\n    repo: https://oauth2:glpat-secret@gitlab.example.com/team/kb.git\n",
			"must not embed credentials",
		},
		"unknown host": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: merge-request\n    repo: https://github.com/example-org/kb.git\n    host: bitbucket\n",
			"update.host must be one of github|gitlab|other",
		},
		"path escapes the contribution repo": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: merge-request\n    repo: https://github.com/example-org/kb.git\n    path: ../../etc\n",
			"must stay inside the contribution repo",
		},
		"absolute in-repo path": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: merge-request\n    repo: https://github.com/example-org/kb.git\n    path: /srv/wiki\n",
			"must be relative to the contribution repo's root",
		},
		"merge-request fields under method: direct": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: direct\n    repo: https://github.com/example-org/kb.git\n    branch: main\n",
			"repo/branch apply to method: merge-request",
		},
		"merge-request fields under method: none": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: none\n    repo: https://github.com/example-org/kb.git\n",
			"repo apply to method: merge-request, not method: none",
		},
		"instructions under method: none": {
			"content:\n  type: local\n  path: kb\n  update:\n    method: none\n    instructions: open a ticket\n",
			"method: none declares that there is none",
		},
		"named collection names the offending path": {
			"collections:\n  - name: handbook\n    type: local\n    path: kb\n    update:\n      method: merge-request\n",
			"collections[handbook].update.repo is required",
		},
		"description is bounded": {
			"content:\n  type: local\n  path: kb\n  description: " + strings.Repeat("x", maxDescriptionLen+1) + "\n",
			"content.description is 501 characters, max 500",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.body), "content-source.yaml")
			if err == nil {
				t.Fatalf("parseConfig succeeded, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseConfig_UpdateInstructionsAreAllowedForDirect: a
// direct-write collection still wants to say where pages go and what to
// run — the mechanics fields are what method: direct has no use for.
func TestParseConfig_UpdateInstructionsAreAllowedForDirect(t *testing.T) {
	cfg, err := parseConfig([]byte("content:\n  type: local\n  path: kb\n  update:\n    method: direct\n    instructions: |\n      Pages live under wiki/. Run `mk list` afterwards.\n"), "c.yaml")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if !strings.Contains(cfg.Content.Update.Instructions, "Pages live under wiki/.") {
		t.Errorf("instructions = %q", cfg.Content.Update.Instructions)
	}
}

func TestResolveRuntimeCollections_CarriesTheContractThrough(t *testing.T) {
	// The resolved collection is what internal/collections builds the
	// registry from, so dropping the contract here would leave every
	// agent guessing again.
	path := writeCfg(t, `
content:
  type: none
  description: The embedded fallback.
  update:
    method: merge-request
    repo: https://github.com/example-org/kb.git
    host: github
    path: wiki
`)
	cols, err := ResolveRuntimeCollections(context.Background(), path)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 1 {
		t.Fatalf("got %d collections, want 1", len(cols))
	}
	src := cols[0].Source
	if src.Description != "The embedded fallback." {
		t.Errorf("description = %q", src.Description)
	}
	if src.Update == nil || src.Update.Repo != "https://github.com/example-org/kb.git" || src.Update.Branch != DefaultUpdateBranch {
		t.Fatalf("update = %+v", src.Update)
	}
}
