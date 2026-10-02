package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testToken is phrase-shaped on purpose: it is not a secret.
const testToken = "not a secret, a test token long enough"

func TestResolve(t *testing.T) {
	for name, tc := range map[string]struct {
		host, repo string
		want       Target
	}{
		"github.com https":    {HostGitHub, "https://github.com/example-org/handbook.git", Target{HostGitHub, "https://api.github.com", "example-org/handbook"}},
		"github.com scp":      {HostGitHub, "git@github.com:example-org/handbook.git", Target{HostGitHub, "https://api.github.com", "example-org/handbook"}},
		"github enterprise":   {HostGitHub, "https://ghe.example.com/team/kb", Target{HostGitHub, "https://ghe.example.com/api/v3", "team/kb"}},
		"gitea ssh with port": {HostGitea, "ssh://git@gitea.example.com:2222/team/kb.git", Target{HostGitea, "https://gitea.example.com/api/v1", "team/kb"}},
		"gitea sub-path":      {HostGitea, "https://example.com/gitea/team/kb.git", Target{HostGitea, "https://example.com/gitea/api/v1", "team/kb"}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Resolve(tc.host, tc.repo)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolve_Rejects(t *testing.T) {
	for name, tc := range map[string]struct {
		host, repo, want string
	}{
		"gitlab":           {HostGitLab, "https://gitlab.com/team/kb.git", "not supported yet"},
		"other":            {"other", "https://forge.example.com/team/kb.git", "not supported yet"},
		"no owner":         {HostGitHub, "https://github.com/kb.git", "names no owner/repo"},
		"github nested":    {HostGitHub, "https://github.com/a/b/c", "owner/repo"},
		"slug":             {HostGitHub, "example-org/kb", "not an https://"},
		"dot segments":     {HostGitea, "https://gitea.example.com/team/../kb", "unsafe path"},
		"ssh without host": {HostGitea, "ssh:///team/kb", "names no host"},
		// SECURITY: nothing in the address may steer the API URL the
		// token is sent to (review of #132).
		"escaped query":       {HostGitHub, "https://github.com/o/r%3Fx=1", "percent-escape"},
		"query":               {HostGitHub, "https://github.com/o/r?x=1", "query"},
		"fragment":            {HostGitea, "https://gitea.example.com/o/r#x", "fragment"},
		"scp host with slash": {HostGitea, "git@evil.example/x:o/r.git", "not a host name"},
		"scp host with space": {HostGitea, "git@evil example:o/r.git", "not a host name"},
		"https bad host":      {HostGitea, "https://gitea_example.com/o/r", "not a host name"},
		"segment chars":       {HostGitHub, "https://github.com/o/r;x", "unsafe path segment"},
		"segment space":       {HostGitea, "https://gitea.example.com/o/r x", "unsafe path segment"},
		"gitea prefix chars":  {HostGitea, "https://gitea.example.com/a@b/o/r", "unsafe path segment"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(tc.host, tc.repo)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Resolve(%q, %q) = %v, want an error containing %q", tc.host, tc.repo, err, tc.want)
			}
		})
	}
	if _, err := Resolve(HostGitLab, "https://gitlab.com/team/kb.git"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("gitlab must be ErrUnsupported, got %v", err)
	}
}

func TestNew(t *testing.T) {
	if _, err := New(Target{Host: HostGitHub}, ""); err == nil {
		t.Error("a client without a token must be refused")
	}
	if c, err := New(Target{Host: HostGitHub, APIBase: "https://api.github.com"}, testToken); err != nil {
		t.Error(err)
	} else if _, ok := c.(*GitHub); !ok {
		t.Errorf("github client = %T", c)
	}
	if c, err := New(Target{Host: HostGitea}, testToken); err != nil {
		t.Error(err)
	} else if _, ok := c.(*Gitea); !ok {
		t.Errorf("gitea client = %T", c)
	}
	if _, err := New(Target{Host: HostGitLab}, testToken); !errors.Is(err, ErrUnsupported) {
		t.Errorf("gitlab: %v", err)
	}
}

// fakeForgeServer serves the handful of endpoints both clients call and
// records what arrived.
type fakeForgeServer struct {
	t          *testing.T
	wantAuth   string
	created    map[string]any
	issueState string
	// labels and list answer a page ("1", "2", …) of labels or issues.
	labels      func(page string) string
	list        func(page string) string
	labelPages  []string
	listQueries []url.Values
}

func (s *fakeForgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != s.wantAuth {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repos/team/kb/issues"):
		if err := json.NewDecoder(r.Body).Decode(&s.created); err != nil {
			s.t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7,"html_url":"https://forge.example.com/team/kb/issues/7","state":"open"}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/team/kb/issues/7"):
		_, _ = w.Write([]byte(`{"number":7,"state":"` + s.issueState + `","labels":[{"name":"needs-human"},{"name":"resolved"}]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/team/kb/labels"):
		s.labelPages = append(s.labelPages, r.URL.Query().Get("page"))
		if s.labels != nil {
			_, _ = w.Write([]byte(s.labels(r.URL.Query().Get("page"))))
			return
		}
		_, _ = w.Write([]byte(`[{"id":3,"name":"needs-human"},{"id":4,"name":"bug"}]`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/team/kb/issues"):
		s.listQueries = append(s.listQueries, r.URL.Query())
		_, _ = w.Write([]byte(s.list(r.URL.Query().Get("page"))))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func TestGitHub_CreateAndState(t *testing.T) {
	ctx := context.Background()
	fs := &fakeForgeServer{t: t, wantAuth: "Bearer " + testToken, issueState: "closed"}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := &GitHub{Base: srv.URL, Token: testToken, HTTP: srv.Client()}

	url, n, err := c.CreateIssue(ctx, "team/kb", "needs-human: x", "body", []string{"needs-human"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 || !strings.HasSuffix(url, "/issues/7") {
		t.Errorf("created %q #%d", url, n)
	}
	if fs.created["title"] != "needs-human: x" || fs.created["body"] != "body" {
		t.Errorf("payload = %v", fs.created)
	}
	if labels, _ := fs.created["labels"].([]any); len(labels) != 1 || labels[0] != "needs-human" {
		t.Errorf("github takes label names: %v", fs.created["labels"])
	}
	state, labels, err := c.IssueState(ctx, "team/kb", 7)
	if err != nil || state != StateClosed || len(labels) != 2 || labels[1] != "resolved" {
		t.Errorf("state = %q %v %v", state, labels, err)
	}
	if _, _, err := c.IssueState(ctx, "team/kb", 8); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing issue: %v", err)
	}
}

func TestGitea_CreateAndState(t *testing.T) {
	ctx := context.Background()
	fs := &fakeForgeServer{t: t, wantAuth: "token " + testToken, issueState: "open"}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := &Gitea{Base: srv.URL, Token: testToken, HTTP: srv.Client()}

	if _, n, err := c.CreateIssue(ctx, "team/kb", "needs-human: x", "body", []string{"needs-human", "unknown-label"}); err != nil || n != 7 {
		t.Fatalf("create: #%d %v", n, err)
	}
	// Gitea takes label IDs; the unknown name is dropped.
	if ids, _ := fs.created["labels"].([]any); len(ids) != 1 || ids[0] != float64(3) {
		t.Errorf("gitea label ids = %v", fs.created["labels"])
	}
	if state, _, err := c.IssueState(ctx, "team/kb", 7); err != nil || state != StateOpen {
		t.Errorf("state = %q %v", state, err)
	}
	// No known label: the issue is filed without labels.
	fs.created = nil
	if _, _, err := c.CreateIssue(ctx, "team/kb", "t", "b", []string{"nope"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.created["labels"]; ok {
		t.Errorf("no label ids expected: %v", fs.created)
	}
}

func TestCall_ErrorsNeverCarryTheToken(t *testing.T) {
	ctx := context.Background()
	fs := &fakeForgeServer{t: t, wantAuth: "Bearer the right one"}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := &GitHub{Base: srv.URL, Token: testToken, HTTP: srv.Client()}
	_, _, err := c.CreateIssue(ctx, "team/kb", "t", "b", nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401, got %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("an error must never carry the token")
	}
	// A long forge message is truncated.
	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 500)))
	}))
	defer long.Close()
	g := &Gitea{Base: long.URL, Token: testToken}
	if _, _, err := g.IssueState(ctx, "team/kb", 1); err == nil || len(err.Error()) > 300 {
		t.Errorf("error = %v", err)
	}
	if _, _, err := g.CreateIssue(ctx, "team/kb", "t", "b", []string{"x"}); err == nil {
		t.Error("a failed label lookup fails the create")
	}
	// Undecodable JSON.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer bad.Close()
	if _, _, err := (&GitHub{Base: bad.URL, Token: testToken}).IssueState(ctx, "team/kb", 1); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("decode error = %v", err)
	}
	// Unreachable.
	if _, _, err := (&GitHub{Base: "http://127.0.0.1:1", Token: testToken}).IssueState(ctx, "team/kb", 1); err == nil {
		t.Error("an unreachable forge is an error")
	}
}

// labelPage renders n labels named l<start>…, plus extra on the page.
func labelPage(start, n int, extra string) string {
	var parts []string
	for i := start; i < start+n; i++ {
		parts = append(parts, fmt.Sprintf(`{"id":%d,"name":"l%d"}`, i, i))
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// A repo with more labels than one page still finds needs-human on a
// later page (review of #132: the first 50 were all that was read).
func TestGitea_LabelLookupPaginates(t *testing.T) {
	ctx := context.Background()
	fs := &fakeForgeServer{t: t, wantAuth: "token " + testToken, labels: func(page string) string {
		switch page {
		case "1":
			return labelPage(100, giteaPageSize, "")
		case "2":
			return labelPage(200, 10, `{"id":3,"name":"needs-human"}`)
		}
		return "[]"
	}}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := &Gitea{Base: srv.URL, Token: testToken, HTTP: srv.Client()}
	if _, _, err := c.CreateIssue(ctx, "team/kb", "t", "b", []string{"needs-human"}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := fs.created["labels"].([]any); len(ids) != 1 || ids[0] != float64(3) {
		t.Errorf("needs-human on page 2 = %v (pages read %v)", fs.created["labels"], fs.labelPages)
	}
	if strings.Join(fs.labelPages, ",") != "1,2" {
		t.Errorf("pages read = %v", fs.labelPages)
	}
	// Found on page 1: no second request.
	fs.labelPages = nil
	fs.labels = func(string) string { return labelPage(100, giteaPageSize-1, `{"id":3,"name":"needs-human"}`) }
	if _, _, err := c.CreateIssue(ctx, "team/kb", "t", "b", []string{"needs-human"}); err != nil {
		t.Fatal(err)
	}
	if len(fs.labelPages) != 1 {
		t.Errorf("pages read = %v", fs.labelPages)
	}
}

const findMarker = "<!-- meerkat:intake-id hb1 -->"

func issueJSON(n int, body string, pr bool) string {
	b, _ := json.Marshal(body)
	extra := ""
	if pr {
		extra = `,"pull_request":{"url":"x"}`
	}
	return fmt.Sprintf(`{"number":%d,"html_url":"https://forge.example.com/team/kb/issues/%d","body":%s%s}`, n, n, b, extra)
}

func TestFindIssue(t *testing.T) {
	ctx := context.Background()
	since := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for name, mk := range map[string]func(base string, hc *http.Client) (Client, string, int){
		"github": func(base string, hc *http.Client) (Client, string, int) {
			return &GitHub{Base: base, Token: testToken, HTTP: hc}, "Bearer " + testToken, gitHubPageSize
		},
		"gitea": func(base string, hc *http.Client) (Client, string, int) {
			return &Gitea{Base: base, Token: testToken, HTTP: hc}, "token " + testToken, giteaPageSize
		},
	} {
		t.Run(name, func(t *testing.T) {
			fs := &fakeForgeServer{t: t}
			srv := httptest.NewServer(fs)
			defer srv.Close()
			c, auth, pageSize := mk(srv.URL, srv.Client())
			fs.wantAuth = auth

			// Page 1 is full of other issues, a pull request carrying the
			// marker, and the marker quoted inside a body (not its first
			// line); page 2 has two real matches, the lower one wins.
			fs.list = func(page string) string {
				switch page {
				case "1":
					items := []string{issueJSON(1, findMarker+"\nbut a pull request", true), issueJSON(2, "quoted:\n"+findMarker, false)}
					for i := 3; len(items) < pageSize; i++ {
						items = append(items, issueJSON(i, "other", false))
					}
					return "[" + strings.Join(items, ",") + "]"
				case "2":
					return "[" + issueJSON(900, findMarker+"\r\nlater copy", false) + "," + issueJSON(500, findMarker+"\nours", false) + "]"
				}
				return "[]"
			}
			u, n, ok, err := c.FindIssue(ctx, "team/kb", findMarker, since)
			if err != nil || !ok || n != 500 || !strings.HasSuffix(u, "/issues/500") {
				t.Fatalf("find = %q #%d %v %v", u, n, ok, err)
			}
			q := fs.listQueries[0]
			if q.Get("state") != "all" || q.Get("since") != "2026-10-02T12:00:00Z" || q.Get("page") != "1" {
				t.Errorf("query = %v", q)
			}
			if name == "gitea" && q.Get("type") != "issues" {
				t.Errorf("gitea lists issues only: %v", q)
			}

			// Nothing matches.
			fs.list = func(string) string { return "[" + issueJSON(1, "other", false) + "]" }
			if _, _, ok, err := c.FindIssue(ctx, "team/kb", findMarker, since); ok || err != nil {
				t.Errorf("no match = %v %v", ok, err)
			}

			// Every page full and no match: truncated, not "absent".
			fs.listQueries = nil
			fs.list = func(page string) string {
				p, _ := strconv.Atoi(page)
				items := make([]string, 0, pageSize)
				for i := 0; i < pageSize; i++ {
					items = append(items, issueJSON(p*1000+i, "other", false))
				}
				return "[" + strings.Join(items, ",") + "]"
			}
			if _, _, ok, err := c.FindIssue(ctx, "team/kb", findMarker, since); ok || !errors.Is(err, ErrSearchTruncated) {
				t.Errorf("truncated = %v %v", ok, err)
			}
			if len(fs.listQueries) != maxPages {
				t.Errorf("read %d pages, want the %d cap", len(fs.listQueries), maxPages)
			}
			// A match on a full last page is still found.
			fs.list = func(page string) string {
				items := []string{issueJSON(7, findMarker, false)}
				for i := 1; i < pageSize; i++ {
					items = append(items, issueJSON(10000+i, "other", false))
				}
				return "[" + strings.Join(items, ",") + "]"
			}
			if _, n, ok, err := c.FindIssue(ctx, "team/kb", findMarker, since); !ok || n != 7 || err != nil {
				t.Errorf("match on capped pages = #%d %v %v", n, ok, err)
			}

			// A forge error is an error.
			fs.wantAuth = "nobody"
			if _, _, _, err := c.FindIssue(ctx, "team/kb", findMarker, since); err == nil || !strings.Contains(err.Error(), "401") {
				t.Errorf("forge error = %v", err)
			}
		})
	}
}
