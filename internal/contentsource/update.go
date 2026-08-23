package contentsource

import (
	"fmt"
	"net/url"
	"strings"
)

// update.go is the per-collection UPDATE CONTRACT: the operator's
// declaration of how knowledge flows back INTO a collection, so an agent
// that learned something has a sanctioned path to contribute it instead
// of guessing. See docs/design/update-contract.md.
//
// The contract is DECLARED, never inferred from the source type. A
// collection served from a GCS mirror may well be maintained in a git
// repository nobody can reach from the serving address, and a local
// directory may be a checkout an operator does not want written to. Only
// the operator knows which address takes contributions, so only the
// operator says.
//
// One rule ties the declaration back to reality: method: direct promises
// that a write lands somewhere the collection actually serves, so it is
// refused at load time for a backend that cannot be written into at all
// (an embedded build, a digest-pinned archive, an immutable bundle).
// That is the only inference here, and it can only ever REJECT a
// declaration — never invent one.

// Update method values for a per-collection `update:` block.
const (
	// UpdateNone declares that there is no sanctioned contribution path.
	// It is also what an absent `update:` block means, which is what
	// every configuration written before this feature has.
	UpdateNone = "none"
	// UpdateDirect declares that a caller who holds the capability may
	// write into the collection's own backend, and that the write is the
	// update. Only legal on a writable backend — see Source.backendKind.
	UpdateDirect = "direct"
	// UpdateMergeRequest declares that contributions reach the collection
	// as a merge/pull request against a contribution repo, reviewed by a
	// human. Legal on ANY backend: the serving mirror and the
	// contribution repo are different addresses by design.
	UpdateMergeRequest = "merge-request"
)

// Contribution host values for a merge-request contract. The host does
// not change what meerkat does — meerkat never talks to a forge — it
// tells an AGENT which CLI mechanics to reach for (`gh pr create`,
// `glab mr create`, or neither).
const (
	UpdateHostGitHub = "github"
	UpdateHostGitLab = "gitlab"
	// UpdateHostOther is the default: a forge meerkat has no opinion
	// about. An agent should follow `instructions:` and plain git.
	UpdateHostOther = "other"
)

// DefaultUpdateBranch is the target branch of a merge-request contract
// that does not name one.
const DefaultUpdateBranch = "main"

// maxDescriptionLen bounds a collection's `description:`. Nothing
// technical requires a bound; the description is rendered into an
// agent's context every time collections are listed, so a
// pathologically long one is a token bill, not a feature. Long-form
// guidance belongs in `update.instructions`, which is only rendered for
// the collection an agent is actually contributing to.
const maxDescriptionLen = 500

// UpdateSpec is the `update:` block of a content-source.yaml collection.
//
//	update:
//	  method: direct            # writable backend only
//
//	update:
//	  method: merge-request
//	  repo: https://github.com/example-org/handbook.git
//	  host: github              # github | gitlab | other
//	  branch: main              # default "main"
//	  path: wiki                # where pages live in the CONTRIBUTION repo
//	  instructions: |
//	    Fork, branch, open a PR. …
//
// Absent (the default), the collection declares no contribution path and
// agents are told so rather than left to guess.
type UpdateSpec struct {
	// Method is the sanctioned contribution path: direct |
	// merge-request | none. Required when the block is present at all —
	// an `update:` block whose method could be defaulted would let a
	// typo'd key silently mean "none".
	Method string `yaml:"method"`

	// Repo is the CONTRIBUTION repo — the address a merge request is
	// opened against, which is deliberately not assumed to be the
	// address the collection is served from. An https:// URL, an ssh://
	// URL, or the scp-like `git@host:owner/repo.git` form; an
	// `owner/repo` slug is refused because it does not name a host.
	//
	// It must NOT embed credentials: the contract is rendered to
	// callers, so a token in this URL is a token handed out.
	Repo string `yaml:"repo,omitempty"`

	// Host selects the CLI mechanics an agent should reach for:
	// github | gitlab | other. Defaults to other.
	Host string `yaml:"host,omitempty"`

	// Branch is the merge request's target branch. Defaults to
	// DefaultUpdateBranch.
	Branch string `yaml:"branch,omitempty"`

	// Path is where wiki pages live WITHIN the contribution repo's
	// layout. It is not derived from `layout.wiki` above: that describes
	// the serving mirror, and the contribution repo may be laid out
	// differently. Relative to the repo root.
	Path string `yaml:"path,omitempty"`

	// Instructions is free-text, agent-facing prose: fork-vs-branch
	// policy, page format expectations, what to run before proposing.
	// Think of it as the skill an operator would otherwise have to
	// explain to every contributor by hand.
	Instructions string `yaml:"instructions,omitempty"`
}

// DeclaredMethod returns the method the operator declared, nil-safe: an
// absent `update:` block is UpdateNone. It is the accessor every reader
// of a contract should use, so "absent" and "explicitly none" cannot
// drift apart.
func (u *UpdateSpec) DeclaredMethod() string {
	if u == nil || u.Method == "" {
		return UpdateNone
	}
	return u.Method
}

// Normalize applies the documented defaults and canonical casing:
// method/host lowercased, surrounding whitespace trimmed, and — for a
// merge-request contract — host and branch defaulted. It runs at load
// time (parseConfig), before Validate, so everything downstream reads a
// fully-formed contract and no surface has to re-apply a default.
func (u *UpdateSpec) Normalize() {
	if u == nil {
		return
	}
	u.Method = strings.ToLower(strings.TrimSpace(u.Method))
	u.Host = strings.ToLower(strings.TrimSpace(u.Host))
	u.Repo = strings.TrimSpace(u.Repo)
	u.Branch = strings.TrimSpace(u.Branch)
	// Trailing slashes only: a LEADING one makes the path absolute, and
	// trimming that would silently turn a mistake into a different,
	// plausible-looking path instead of the error it gets.
	u.Path = strings.TrimRight(strings.TrimSpace(u.Path), "/")
	u.Instructions = strings.TrimSpace(u.Instructions)
	if u.Method != UpdateMergeRequest {
		return
	}
	if u.Host == "" {
		u.Host = UpdateHostOther
	}
	if u.Branch == "" {
		u.Branch = DefaultUpdateBranch
	}
}

// Validate checks an `update:` block's shape. label is the config path a
// message should name, e.g. "collections[handbook].update".
//
// It deliberately does NOT check the direct-on-a-read-only-backend rule:
// that one needs the collection's source, and lives in
// Source.validateContract.
//
// A field that belongs to another method is an ERROR rather than an
// ignored line. An operator who wrote `repo:` under `method: direct`
// believes contributions go to that repo; silently dropping it would
// make the contract meerkat renders differ from the one they read.
func (u *UpdateSpec) Validate(label string) error {
	if u == nil {
		return nil
	}
	switch u.Method {
	case "":
		return fmt.Errorf("%s.method is required — one of %s|%s|%s (omit the whole update: block to declare no contribution path)",
			label, UpdateDirect, UpdateMergeRequest, UpdateNone)
	case UpdateNone:
		if fields := u.mergeRequestFields(); len(fields) > 0 {
			return fmt.Errorf("%s: %s apply to method: %s, not method: %s", label, strings.Join(fields, "/"), UpdateMergeRequest, UpdateNone)
		}
		if u.Instructions != "" {
			return fmt.Errorf("%s: instructions describe a contribution path, and method: %s declares that there is none — "+
				"use method: %s or method: %s if there is one", label, UpdateNone, UpdateDirect, UpdateMergeRequest)
		}
	case UpdateDirect:
		if fields := u.mergeRequestFields(); len(fields) > 0 {
			return fmt.Errorf("%s: %s apply to method: %s — method: %s writes into the collection's own backend, so there is no contribution repo to name",
				label, strings.Join(fields, "/"), UpdateMergeRequest, UpdateDirect)
		}
	case UpdateMergeRequest:
		if u.Repo == "" {
			return fmt.Errorf("%s.repo is required for method: %s — the address a merge request is opened against, "+
				"which is not assumed to be the address this collection is served from", label, UpdateMergeRequest)
		}
		if err := validateContributionRepo(label, u.Repo); err != nil {
			return err
		}
		switch u.Host {
		case UpdateHostGitHub, UpdateHostGitLab, UpdateHostOther, "":
		default:
			return fmt.Errorf("%s.host must be one of %s|%s|%s, got %q — it selects the CLI mechanics an agent reaches for, not a network endpoint",
				label, UpdateHostGitHub, UpdateHostGitLab, UpdateHostOther, u.Host)
		}
		if err := validateContributionPath(label, u.Path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s.method must be one of %s|%s|%s, got %q", label, UpdateDirect, UpdateMergeRequest, UpdateNone, u.Method)
	}
	return nil
}

// mergeRequestFields names the set fields that only mean something for a
// merge-request contract, so a rejection can list exactly what was
// misplaced.
func (u *UpdateSpec) mergeRequestFields() []string {
	fields := make([]string, 0, 4)
	if u.Repo != "" {
		fields = append(fields, "repo")
	}
	if u.Host != "" {
		fields = append(fields, "host")
	}
	if u.Branch != "" {
		fields = append(fields, "branch")
	}
	if u.Path != "" {
		fields = append(fields, "path")
	}
	return fields
}

// validateContributionRepo checks that repo names a host and carries no
// credentials.
//
// The credential check is the security-relevant half: an update contract
// is rendered to every caller who can see the collection, so a
// `https://user:token@host/...` repo URL would be a token published to
// them all. It is refused at load time, where an operator sees it,
// rather than redacted later on one surface and forgotten on the next.
func validateContributionRepo(label, repo string) error {
	switch {
	case strings.HasPrefix(repo, "https://"), strings.HasPrefix(repo, "ssh://"):
		u, err := url.Parse(repo)
		if err != nil {
			return fmt.Errorf("%s.repo is not a valid URL: %v", label, err)
		}
		if u.Host == "" {
			return fmt.Errorf("%s.repo names no host: %q", label, repo)
		}
		// An ssh:// URL's user is an SSH login ("git"), which is not a
		// secret; an https:// URL's userinfo is a credential in every
		// realistic case, and a password is one in both.
		if u.User != nil {
			_, hasPassword := u.User.Password()
			if hasPassword || u.Scheme == "https" {
				return fmt.Errorf("%s.repo must not embed credentials — the update contract is rendered to every caller who can see this collection, "+
					"so a token in this URL is a token handed to them; use a plain https:// URL or an ssh remote and let the contributor's own credentials apply", label)
			}
		}
	case scpLikeRepo(repo):
		// git@github.com:example-org/handbook.git — the user part of an
		// scp-like remote is an SSH login ("git"), not a secret.
	default:
		return fmt.Errorf("%s.repo must be an https:// URL, an ssh:// URL, or the git@host:owner/repo form, got %q — "+
			"an owner/repo slug names no host, and the contribution repo need not be on the same host as the served content", label, repo)
	}
	return nil
}

// scpLikeRepo reports whether repo is the scp-like SSH remote form,
// `user@host:path` — a shape url.Parse does not recognise.
func scpLikeRepo(repo string) bool {
	if strings.Contains(repo, "://") {
		return false
	}
	at := strings.Index(repo, "@")
	colon := strings.Index(repo, ":")
	return at > 0 && colon > at+1 && colon < len(repo)-1
}

// validateContributionPath checks the in-repo page path. meerkat never
// resolves it — it is prose handed to an agent — but an agent WILL join
// it onto a clone directory, so an absolute path or a `..` segment is
// refused here rather than becoming that agent's problem.
func validateContributionPath(label, p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") || (len(p) > 1 && p[1] == ':') {
		return fmt.Errorf("%s.path must be relative to the contribution repo's root, got %q", label, p)
	}
	for _, seg := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if seg == ".." {
			return fmt.Errorf("%s.path must stay inside the contribution repo, got %q", label, p)
		}
	}
	return nil
}

// backendKind describes the collection's backing store for the
// direct-writability rule: whether a page written to it would land
// somewhere this collection actually serves, and how to name it in an
// error.
//
// Writable means the same thing it means for a `memory:` store (see
// internal/memory.Spec): a DIRECTORY on disk, or a GCS object PREFIX.
// Everything else is a snapshot — a digest-pinned archive, an immutable
// bundle, content embedded at build time, or a build-time-only git
// source — and a write to it either has nowhere to go or is replaced by
// the next content resolution.
func (s Source) backendKind() (writable bool, desc string) {
	switch s.Type {
	case TypeLocal:
		return true, "type: local (a directory on disk)"
	case TypeGCS:
		if s.Prefix != "" {
			return true, "type: gcs with prefix: (an object prefix served as a directory tree)"
		}
		return false, "type: gcs with object: (one immutable .tar.gz bundle, pinned by generation)"
	case TypeURL:
		return false, "type: url (a digest-pinned .tar.gz archive)"
	case TypeNone:
		return false, "type: none (the content embedded in the binary at build time)"
	case TypeGit, TypeSubmodule:
		return false, fmt.Sprintf("type: %s (build-time only — what is served is a copy embedded at build time)", s.Type)
	default:
		return false, "type: " + s.Type
	}
}

// validateContract checks the collection-context parts of the update
// contract: the description bound, and the one rule that needs the
// source — method: direct on a backend that cannot be written into.
//
// The direct rule fails STARTUP, not the first write. A contract is a
// promise made to agents, and a promise that resolves to "your write
// went into a cache directory that is replaced on the next content
// change" is discovered as lost work, days later, by whoever wrote it.
func (s Source) validateContract(p string) error {
	if len(s.Description) > maxDescriptionLen {
		return fmt.Errorf("%s.description is %d characters, max %d — it is rendered into an agent's context every time collections are listed; "+
			"put long-form guidance in update.instructions instead", p, len(s.Description), maxDescriptionLen)
	}
	if err := s.Update.Validate(p + ".update"); err != nil {
		return err
	}
	if s.Update.DeclaredMethod() != UpdateDirect {
		return nil
	}
	writable, desc := s.backendKind()
	if writable {
		return nil
	}
	return fmt.Errorf("%s.update: method: %s needs a writable backend, but this collection is %s — a direct write has nowhere to land that this collection would serve; "+
		"declare method: %s naming the repo this mirror is built from, or method: %s",
		p, UpdateDirect, desc, UpdateMergeRequest, UpdateNone)
}
