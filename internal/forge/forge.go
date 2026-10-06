// Package forge files and reads issues on a collection's forge: the one
// place meerkat talks to GitHub or Gitea itself (meerkat-mob #19).
//
// The librarian uses it to escalate a parked (needs-human) intake item
// to the people who own the target collection, where they already work,
// and to learn when they have resolved it. Everything else about a
// forge stays advice to an agent (docs/design/update-contract.md).
//
// Three rules shape the package:
//
//   - The token comes from the caller, who reads it from the
//     environment variable the update contract names (token_env). It is
//     never read from config, never logged, and never part of an error.
//   - net/http directly, one bounded timeout, bounded response reads, no
//     SDK: a handful of REST calls per forge do not justify a dependency.
//   - The API endpoint is derived from the contract's repo URL, which the
//     operator declared; nothing here follows an address a page supplied.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Host kinds, the same values as contentsource.UpdateHost*.
const (
	HostGitHub = "github"
	HostGitLab = "gitlab"
	HostGitea  = "gitea"
)

// Timeout bounds every forge request.
const Timeout = 30 * time.Second

// maxResponseBytes bounds how much of a forge response is read.
const maxResponseBytes = 1 << 20

// Issue states as IssueState reports them.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

// ErrUnsupported is returned for a host this package cannot file on yet.
var ErrUnsupported = errors.New("forge host not supported yet")

// ErrSearchTruncated is returned by FindIssue when more issues changed
// since the given time than it reads: an earlier filing could not be
// ruled out.
var ErrSearchTruncated = errors.New("too many issues changed to search them all")

// Paging bounds. GitHub serves at most 100 items a page; Gitea's default
// MAX_RESPONSE_ITEMS is 50. maxPages bounds every listing, so a busy
// repo costs a bounded number of requests.
const (
	gitHubPageSize = 100
	giteaPageSize  = 50
	maxPages       = 10
)

// Client files and reads issues on one forge. repo is the "owner/repo"
// slug (Target.Repo).
type Client interface {
	// CreateIssue opens an issue and returns its web URL and number.
	// Labels the forge does not know may be dropped rather than fail.
	CreateIssue(ctx context.Context, repo, title, body string, labels []string) (url string, number int, err error)
	// IssueState returns "open" or "closed" and the issue's label names.
	IssueState(ctx context.Context, repo string, number int) (state string, labels []string, err error)
	// FindIssue returns the lowest-numbered issue (open or closed, never
	// a pull request) updated at or after since whose body's first line
	// is exactly marker; found is false when there is none. When more
	// issues changed than it reads, it returns ErrSearchTruncated.
	FindIssue(ctx context.Context, repo, marker string, since time.Time) (url string, number int, found bool, err error)
}

// Target is where a contract's issues go.
type Target struct {
	// Host is the forge kind: github | gitlab | gitea.
	Host string
	// APIBase is the REST root, e.g. https://api.github.com or
	// https://gitea.example.com/api/v1.
	APIBase string
	// Repo is the owner/repo slug.
	Repo string
}

// Resolve derives the issue target from a contract's host and repo URL
// (https://host/owner/repo.git, ssh://git@host/owner/repo.git or
// git@host:owner/repo.git). An SSH remote's port is the SSH daemon's,
// not the web server's, so it is dropped; the API is always https.
//
// SECURITY: the host and every path segment are checked against a
// narrow character set before anything is built from them: the result
// is spliced into API URLs that carry the token, so a '?', '#', '%' or
// '/' in the wrong place would change which endpoint the token is sent
// to.
func Resolve(host, repoURL string) (Target, error) {
	if strings.ContainsAny(repoURL, "?#%") {
		return Target{}, fmt.Errorf("repo %q: a query, fragment or percent-escape is not part of a repo address", repoURL)
	}
	webHost, repoPath, err := splitRepoURL(repoURL)
	if err != nil {
		return Target{}, err
	}
	if !hostPattern.MatchString(webHost) {
		return Target{}, fmt.Errorf("repo %q: host %q is not a host name", repoURL, webHost)
	}
	segs := strings.Split(strings.Trim(strings.TrimSuffix(repoPath, ".git"), "/"), "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || !segmentPattern.MatchString(s) {
			return Target{}, fmt.Errorf("repo %q: unsafe path segment %q (letters, digits, '.', '_' and '-' only)", repoURL, s)
		}
	}
	if len(segs) < 2 {
		return Target{}, fmt.Errorf("repo %q names no owner/repo", repoURL)
	}
	slug := strings.Join(segs[len(segs)-2:], "/")
	prefix := strings.Join(segs[:len(segs)-2], "/")
	switch host {
	case HostGitHub:
		if prefix != "" {
			return Target{}, fmt.Errorf("repo %q: a GitHub repo is owner/repo", repoURL)
		}
		base := "https://" + webHost + "/api/v3" // GitHub Enterprise Server
		if webHost == "github.com" || webHost == "www.github.com" {
			base = "https://api.github.com"
		}
		return Target{Host: host, APIBase: base, Repo: slug}, nil
	case HostGitea:
		// Gitea may be served under a sub-path: everything before
		// owner/repo is that path.
		base := "https://" + webHost
		if prefix != "" {
			base += "/" + prefix
		}
		return Target{Host: host, APIBase: base + "/api/v1", Repo: slug}, nil
	case HostGitLab:
		return Target{}, fmt.Errorf("%w: gitlab (file the issue by hand)", ErrUnsupported)
	default:
		return Target{}, fmt.Errorf("%w: host %q (declare github or gitea)", ErrUnsupported, host)
	}
}

var (
	// hostPattern is a DNS name with an optional port (https only; an
	// SSH port is dropped before the check).
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	// segmentPattern is one owner, repo or sub-path segment.
	segmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func splitRepoURL(repoURL string) (host, repoPath string, err error) {
	switch {
	case strings.HasPrefix(repoURL, "https://"), strings.HasPrefix(repoURL, "ssh://"):
		u, err := url.Parse(repoURL)
		if err != nil {
			return "", "", fmt.Errorf("repo %q: %w", repoURL, err)
		}
		host = u.Host
		if u.Scheme == "ssh" {
			host = u.Hostname()
		}
		if host == "" {
			return "", "", fmt.Errorf("repo %q names no host", repoURL)
		}
		return host, u.Path, nil
	default:
		// scp-like: user@host:owner/repo.git
		at := strings.Index(repoURL, "@")
		colon := strings.Index(repoURL, ":")
		if at <= 0 || colon <= at+1 || colon == len(repoURL)-1 || strings.Contains(repoURL, "://") {
			return "", "", fmt.Errorf("repo %q is not an https://, ssh:// or git@host:owner/repo address", repoURL)
		}
		return repoURL[at+1 : colon], repoURL[colon+1:], nil
	}
}

// New returns the client for a resolved target.
func New(t Target, token string) (Client, error) {
	if token == "" {
		return nil, errors.New("no forge token")
	}
	hc := &http.Client{Timeout: Timeout}
	switch t.Host {
	case HostGitHub:
		return &GitHub{Base: t.APIBase, Token: token, HTTP: hc}, nil
	case HostGitea:
		return &Gitea{Base: t.APIBase, Token: token, HTTP: hc}, nil
	default:
		return nil, fmt.Errorf("%w: host %q", ErrUnsupported, t.Host)
	}
}

// GitHub is a Client over the GitHub REST API (github.com or GitHub
// Enterprise Server).
type GitHub struct {
	Base  string
	Token string
	HTTP  *http.Client
}

type issueResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (r issueResponse) labelNames() []string {
	out := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		out = append(out, l.Name)
	}
	return out
}

func (g *GitHub) auth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// CreateIssue implements Client. GitHub creates labels it does not
// know when the token may, and drops them silently when it may not.
func (g *GitHub) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (string, int, error) {
	payload := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		payload["labels"] = labels
	}
	var out issueResponse
	if err := call(ctx, g.HTTP, g.auth, http.MethodPost, g.Base+"/repos/"+repo+"/issues", payload, &out); err != nil {
		return "", 0, err
	}
	return out.HTMLURL, out.Number, nil
}

// IssueState implements Client.
func (g *GitHub) IssueState(ctx context.Context, repo string, number int) (string, []string, error) {
	var out issueResponse
	if err := call(ctx, g.HTTP, g.auth, http.MethodGet, fmt.Sprintf("%s/repos/%s/issues/%d", g.Base, repo, number), nil, &out); err != nil {
		return "", nil, err
	}
	return strings.ToLower(out.State), out.labelNames(), nil
}

// Gitea is a Client over the Gitea (and Forgejo) REST API.
type Gitea struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func (g *Gitea) auth(req *http.Request) {
	req.Header.Set("Authorization", "token "+g.Token)
	req.Header.Set("Accept", "application/json")
}

// CreateIssue implements Client. Gitea takes label IDs, not names, so
// the names are looked up in the repo; a name the repo does not have is
// dropped rather than created (creating labels needs more than
// issue-write).
func (g *Gitea) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (string, int, error) {
	payload := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		ids, err := g.labelIDs(ctx, repo, labels)
		if err != nil {
			return "", 0, err
		}
		if len(ids) > 0 {
			payload["labels"] = ids
		}
	}
	var out issueResponse
	if err := call(ctx, g.HTTP, g.auth, http.MethodPost, g.Base+"/repos/"+repo+"/issues", payload, &out); err != nil {
		return "", 0, err
	}
	return out.HTMLURL, out.Number, nil
}

// labelIDs looks the names up page by page, stopping once every name
// is found or the repo runs out of labels.
func (g *Gitea) labelIDs(ctx context.Context, repo string, names []string) ([]int64, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var ids []int64
	for page := 1; page <= maxPages && len(want) > 0; page++ {
		var have []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		endpoint := fmt.Sprintf("%s/repos/%s/labels?limit=%d&page=%d", g.Base, repo, giteaPageSize, page)
		if err := call(ctx, g.HTTP, g.auth, http.MethodGet, endpoint, nil, &have); err != nil {
			return nil, err
		}
		for _, l := range have {
			if want[l.Name] {
				ids = append(ids, l.ID)
				delete(want, l.Name)
			}
		}
		if len(have) < giteaPageSize {
			break
		}
	}
	return ids, nil
}

// IssueState implements Client.
func (g *Gitea) IssueState(ctx context.Context, repo string, number int) (string, []string, error) {
	var out issueResponse
	if err := call(ctx, g.HTTP, g.auth, http.MethodGet, fmt.Sprintf("%s/repos/%s/issues/%d", g.Base, repo, number), nil, &out); err != nil {
		return "", nil, err
	}
	return strings.ToLower(out.State), out.labelNames(), nil
}

// listedIssue is one entry of an issue listing.
type listedIssue struct {
	Number      int             `json:"number"`
	HTMLURL     string          `json:"html_url"`
	Body        string          `json:"body"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// findIssue pages through a listing (page numbers from 1) for the
// lowest-numbered issue whose body's first line is marker.
func findIssue(ctx context.Context, marker string, pageSize int, list func(ctx context.Context, page int) ([]listedIssue, error)) (string, int, bool, error) {
	var best *listedIssue
	for page := 1; page <= maxPages; page++ {
		items, err := list(ctx, page)
		if err != nil {
			return "", 0, false, err
		}
		for i := range items {
			is := &items[i]
			if len(is.PullRequest) > 0 && string(is.PullRequest) != "null" {
				continue
			}
			first, _, _ := strings.Cut(is.Body, "\n")
			if strings.TrimRight(first, "\r") != marker {
				continue
			}
			if best == nil || is.Number < best.Number {
				best = is
			}
		}
		if len(items) < pageSize {
			if best == nil {
				return "", 0, false, nil
			}
			return best.HTMLURL, best.Number, true, nil
		}
	}
	if best != nil {
		return best.HTMLURL, best.Number, true, nil
	}
	return "", 0, false, fmt.Errorf("%w (more than %d pages)", ErrSearchTruncated, maxPages)
}

// FindIssue implements Client over GET /repos/{repo}/issues, which is
// read from the database, not a search index, so an issue created a
// moment ago is already listed.
func (g *GitHub) FindIssue(ctx context.Context, repo, marker string, since time.Time) (string, int, bool, error) {
	return findIssue(ctx, marker, gitHubPageSize, func(ctx context.Context, page int) ([]listedIssue, error) {
		var out []listedIssue
		endpoint := fmt.Sprintf("%s/repos/%s/issues?state=all&sort=created&direction=asc&since=%s&per_page=%d&page=%d",
			g.Base, repo, url.QueryEscape(since.UTC().Format(time.RFC3339)), gitHubPageSize, page)
		err := call(ctx, g.HTTP, g.auth, http.MethodGet, endpoint, nil, &out)
		return out, err
	})
}

// FindIssue implements Client over GET /repos/{repo}/issues?type=issues.
func (g *Gitea) FindIssue(ctx context.Context, repo, marker string, since time.Time) (string, int, bool, error) {
	return findIssue(ctx, marker, giteaPageSize, func(ctx context.Context, page int) ([]listedIssue, error) {
		var out []listedIssue
		endpoint := fmt.Sprintf("%s/repos/%s/issues?state=all&type=issues&since=%s&limit=%d&page=%d",
			g.Base, repo, url.QueryEscape(since.UTC().Format(time.RFC3339)), giteaPageSize, page)
		err := call(ctx, g.HTTP, g.auth, http.MethodGet, endpoint, nil, &out)
		return out, err
	})
}

// call does one JSON request. The error names the method, the API path
// and the status with a short excerpt of the forge's message; never the
// token.
func call(ctx context.Context, hc *http.Client, auth func(*http.Request), method, endpoint string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	auth(req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if hc == nil {
		hc = &http.Client{Timeout: Timeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return fmt.Errorf("%s %s: %s: %s", method, req.URL.Path, resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, req.URL.Path, err)
	}
	return nil
}
