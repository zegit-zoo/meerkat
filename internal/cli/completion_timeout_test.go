package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
)

// TestComplete_RemoteContentSourceIsBounded pins the #78 review
// follow-up: a TAB whose --content-source points at a remote store that
// never answers must not hold the shell. The re-resolution runs under
// completionResolveTimeout, and past it the TAB offers no completions —
// silently, with nothing but the protocol on stdout.
func TestComplete_RemoteContentSourceIsBounded(t *testing.T) {
	isolateCompletionEnv(t)

	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// A store that accepted the connection and then went quiet.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	trustTestServer(t, srv)

	orig := completionResolveTimeout
	completionResolveTimeout = 200 * time.Millisecond
	t.Cleanup(func() { completionResolveTimeout = orig })

	cfg := filepath.Join(t.TempDir(), contentsource.ConfigFile)
	write(t, cfg, "content:\n  type: url\n  url: "+srv.URL+"/kb.tar.gz\n  sha256: "+strings.Repeat("ab", 32)+"\n")

	start := time.Now()
	stdout, _ := execComplete(t, "show", "--content-source", cfg, "con")
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("a TAB against an unresponsive store took %s; want it bounded near %s", elapsed, completionResolveTimeout)
	}
	got, directive := splitCompletions(t, stdout)
	if len(got) != 0 {
		t.Errorf("completions from content that never arrived: %v", got)
	}
	if want := ":" + strconv.Itoa(int(cobra.ShellCompDirectiveNoFileComp)); directive != want {
		t.Errorf("directive = %q, want %q", directive, want)
	}
}
