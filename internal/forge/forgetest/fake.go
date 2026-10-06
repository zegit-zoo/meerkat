// Package forgetest provides an in-memory forge.Client for tests.
//
// It lives in its own package, as authntest does, so the fake never
// ships in the mk binary: only test files import it.
package forgetest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zegit-zoo/meerkat/internal/forge"
)

// Fake is an in-memory forge.Client: no network, no token. It numbers
// issues per fake from 1 and records what was filed.
type Fake struct {
	mu     sync.Mutex
	issues []*Issue
	// Err, when set, fails every call.
	Err error
}

var _ forge.Client = (*Fake)(nil)

// Issue is one issue a Fake holds.
type Issue struct {
	Repo   string
	Number int
	Title  string
	Body   string
	Labels []string
	State  string
}

// CreateIssue implements forge.Client.
func (f *Fake) CreateIssue(_ context.Context, repo, title, body string, labels []string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", 0, f.Err
	}
	n := len(f.issues) + 1
	f.issues = append(f.issues, &Issue{Repo: repo, Number: n, Title: title, Body: body, Labels: append([]string(nil), labels...), State: forge.StateOpen})
	return fmt.Sprintf("https://forge.invalid/%s/issues/%d", repo, n), n, nil
}

// IssueState implements forge.Client.
func (f *Fake) IssueState(_ context.Context, repo string, number int) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", nil, f.Err
	}
	for _, is := range f.issues {
		if is.Repo == repo && is.Number == number {
			return is.State, append([]string(nil), is.Labels...), nil
		}
	}
	return "", nil, fmt.Errorf("issue %s#%d not found", repo, number)
}

// FindIssue implements forge.Client. The fake keeps no timestamps, so
// since is ignored: every issue counts as recent.
func (f *Fake) FindIssue(_ context.Context, repo, marker string, _ time.Time) (string, int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", 0, false, f.Err
	}
	for _, is := range f.issues {
		first, _, _ := strings.Cut(is.Body, "\n")
		if is.Repo == repo && first == marker {
			return fmt.Sprintf("https://forge.invalid/%s/issues/%d", repo, is.Number), is.Number, true, nil
		}
	}
	return "", 0, false, nil
}

// Close closes an issue and adds labels, as a human on the forge would.
func (f *Fake) Close(repo string, number int, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, is := range f.issues {
		if is.Repo == repo && is.Number == number {
			is.State = forge.StateClosed
			is.Labels = append(is.Labels, labels...)
		}
	}
}

// Issues returns a copy of every issue filed so far.
func (f *Fake) Issues() []Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Issue, 0, len(f.issues))
	for _, is := range f.issues {
		out = append(out, *is)
	}
	return out
}
