package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/auth"
)

// Project is the GitHub repository meerkat releases ship from.
const Project = "zegit-zoo/meerkat"

// projectOwner and projectRepo split Project into its two path segments.
// Anything after the first "/" is the repo, so a malformed constant can
// never expand into extra path elements.
func projectOwner() string {
	owner, _, _ := strings.Cut(Project, "/")
	return owner
}

func projectRepo() string {
	_, repo, _ := strings.Cut(Project, "/")
	return repo
}

// githubAPIBase is the GitHub API origin; overridable in tests so
// fetchOne can be pointed at an httptest server.
var githubAPIBase = "https://api.github.com"

// resolveGitHubToken returns a cached gh CLI OAuth token, if any. It's a
// package-level var (rather than a direct auth.NewDefault() call) purely
// so tests can stub it without shelling out to a real `gh` binary or
// depending on the test machine's own gh login state.
var resolveGitHubToken = func() (string, error) {
	return auth.NewDefault().Token(auth.HostGitHub, "github.com")
}

// Release represents the subset of GitHub release metadata we use.
type Release struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	PublishedAt string  `json:"published_at"`
	Assets      []Asset `json:"assets"`
}

// Asset is one downloadable artifact attached to a release.
type Asset struct {
	Name string `json:"name"`
	// URL is the GitHub API asset URL, used for authenticated download.
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// FetchLatest returns the most recent release on the project.
//
// The repository is public, so reading release metadata works
// anonymously. If a gh CLI OAuth token is cached (via internal/auth) we
// send it anyway, purely for the higher authenticated GitHub API rate
// limit; its absence is not an error.
func FetchLatest(ctx context.Context) (*Release, error) {
	return fetchOne(ctx, "")
}

// FetchByTag returns a specific release by tag name.
func FetchByTag(ctx context.Context, tag string) (*Release, error) {
	return fetchOne(ctx, tag)
}

func fetchOne(ctx context.Context, tag string) (*Release, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// A cached token is optional: the repo is public, so anonymous
	// requests work fine. When present we still send it, for the
	// higher authenticated rate limit (60/hr anonymous vs 5000/hr).
	tok, tokErr := resolveGitHubToken()
	if tokErr != nil {
		tok = ""
	}

	// Project is "owner/repo", i.e. two path segments. url.PathEscape
	// escapes "/" to %2F, which GitHub does not resolve — every request
	// 404s and the CLI reports "no releases yet" no matter what has been
	// published. Escape each segment and rejoin, so a segment containing
	// a slash still cannot smuggle in extra path elements.
	var u string
	projectPath := path.Join(url.PathEscape(projectOwner()), url.PathEscape(projectRepo()))
	if tag != "" {
		u = githubAPIBase + "/repos/" + projectPath + "/releases/tags/" + url.PathEscape(tag)
	} else {
		u = githubAPIBase + "/repos/" + projectPath + "/releases/latest"
	}

	req, err := http.NewRequestWithContext(c, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setUA(req)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		if tag != "" {
			return nil, fmt.Errorf("release %q not found", tag)
		}
		return nil, fmt.Errorf("no releases yet on %s", Project)
	case http.StatusUnauthorized, http.StatusForbidden:
		if tok != "" {
			return nil, fmt.Errorf("GitHub returned %s — run `gh auth login` (or `gh auth status`) to refresh your token", resp.Status)
		}
		return nil, fmt.Errorf("GitHub returned %s — this is likely the anonymous API rate limit; run `gh auth login` for a higher limit", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &rel, nil
}

// PickAssetName returns the asset name for the current OS/arch.
//
// Goreleaser's `{{.Version}}` strips the leading `v` from the tag,
// so the assets are named:
//
//	meerkat_<X.Y.Z>_<os>_<arch>.tar.gz
//
// and a sibling checksum file:
//
//	meerkat_<X.Y.Z>_checksums.txt
func PickAssetName(version string) string {
	v := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("meerkat_%s_%s_%s.tar.gz",
		v, runtime.GOOS, runtime.GOARCH)
}

// ChecksumAssetName returns the checksum file name for a release.
func ChecksumAssetName(version string) string {
	v := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("meerkat_%s_checksums.txt", v)
}

// FindAsset searches the release assets for one whose Name matches
// name and returns its API URL. The API URL is used for authenticated
// download (with Accept: application/octet-stream). Returns false if
// no matching asset is found.
func (r *Release) FindAsset(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, true
		}
	}
	return "", false
}

func setUA(req *http.Request) {
	req.Header.Set("User-Agent",
		fmt.Sprintf("meerkat-update (%s/%s)", runtime.GOOS, runtime.GOARCH))
}
