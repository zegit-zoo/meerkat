//go:build linux

package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// activation_linux_test.go pins #110: serve-http serves on the socket
// systemd passes in, follows sd_listen_fds(3), and refuses what it cannot
// serve correctly.

// fakeEnv is an environment for activationEnv that records unsets.
type fakeEnv map[string]string

func (f fakeEnv) get(k string) string { return f[k] }
func (f fakeEnv) unset(k string) error {
	delete(f, k)
	return nil
}

// dupFD returns a new descriptor for f's socket, which listener() may
// consume (it closes what it is given).
func dupFD(t *testing.T, f *os.File) int {
	t.Helper()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func tcpListenerFile(t *testing.T) (*net.TCPListener, *os.File) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tl := l.(*net.TCPListener)
	f, err := tl.File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return tl, f
}

func TestActivation_NoVariablesMeansBindAsBefore(t *testing.T) {
	env := fakeEnv{"PATH": "/usr/bin"}
	ln, err := activationEnv{getenv: env.get, unsetenv: env.unset, pid: 42, fd: 99}.listener()
	if ln != nil || err != nil {
		t.Fatalf("listener() = %v, %v; want nil, nil", ln, err)
	}
	if env["PATH"] != "/usr/bin" {
		t.Error("an unrelated variable was touched")
	}
}

// A parent's LISTEN_* variables describe the parent's sockets. They are
// ignored, and still unset so they go no further.
func TestActivation_AnotherProcessesVariablesAreIgnoredAndCleared(t *testing.T) {
	env := fakeEnv{"LISTEN_PID": "7", "LISTEN_FDS": "1", "LISTEN_FDNAMES": "x"}
	ln, err := activationEnv{getenv: env.get, unsetenv: env.unset, pid: 42, fd: 99}.listener()
	if ln != nil || err != nil {
		t.Fatalf("listener() = %v, %v; want nil, nil for another PID's sockets", ln, err)
	}
	if len(env) != 0 {
		t.Errorf("LISTEN_* left in the environment: %v", env)
	}
}

func TestActivation_Refusals(t *testing.T) {
	cases := []struct {
		name, fds, want string
	}{
		{"several sockets", "2", "passed 2 sockets"},
		{"not a number", "one", "not a socket count"},
		{"negative", "-1", "not a socket count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := fakeEnv{"LISTEN_PID": "42", "LISTEN_FDS": tc.fds}
			_, err := activationEnv{getenv: env.get, unsetenv: env.unset, pid: 42, fd: 99}.listener()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Accept=yes passes one CONNECTED socket per client. Serving on it would
// fail at the first Accept, so it is refused up front with the fix named.
func TestActivation_AConnectedSocketIsRefused(t *testing.T) {
	tl, _ := tcpListenerFile(t)
	conn, err := net.Dial("tcp", tl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cf, err := conn.(*net.TCPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	defer cf.Close()

	env := fakeEnv{"LISTEN_PID": "42", "LISTEN_FDS": "1"}
	_, err = activationEnv{getenv: env.get, unsetenv: env.unset, pid: 42, fd: dupFD(t, cf)}.listener()
	if err == nil || !strings.Contains(err.Error(), "Accept=no") {
		t.Fatalf("err = %v, want a refusal naming Accept=no", err)
	}
}

func TestActivation_ANonSocketIsRefused(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()
	env := fakeEnv{"LISTEN_PID": "42", "LISTEN_FDS": "1"}
	_, err = activationEnv{getenv: env.get, unsetenv: env.unset, pid: 42, fd: dupFD(t, r)}.listener()
	if err == nil || !strings.Contains(err.Error(), "not a listening socket") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// The hosted server serves on a handed-over listener: /livez answers
// through it, Addr reports it, and DNS-rebinding protection still holds
// on a loopback socket (it reads each connection's local address).
func TestHosted_ServesOnAHandedOverListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := collections.New(collections.FromPages("kb", []kb.Page{testPage("a", "A", "body", "", "", "")}))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewHosted(context.Background(), HostedConfig{
		Addr:        "127.0.0.1:1", // must be ignored
		Listener:    l,
		Collections: reg,
		Version:     "test",
		Logger:      slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if !srv.SocketActivated() || srv.Addr() != l.Addr().String() {
		t.Errorf("Addr = %q (activated %v), want the handed-over %q", srv.Addr(), srv.SocketActivated(), l.Addr())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	base := "http://" + l.Addr().String()
	resp, err := http.Get(base + LivenessPath)
	if err != nil {
		t.Fatalf("GET /livez through the handed-over socket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/livez = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, base+DefaultEndpointPath, strings.NewReader(`{}`))
	req.Host = "attacker.example"
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a rebound Host on a loopback socket got %d, want 403", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ListenAndServe = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
}

// The real protocol end to end: a child process gets the socket as fd 3,
// the way systemd passes it, and serves on it.
//
// systemd sets LISTEN_PID after fork, to the child's own PID, which
// exec.Cmd cannot do; the helper sets it on itself before reading the
// variables, which is the same state ActivationListener sees under
// systemd.
//
// OPT-IN (MEERKAT_TEST_EXEC=1), because it executes the test binary again
// from its build directory. On a runner watched by Falco that is the
// "drop and execute a new binary" pattern, which pages. The tests above
// cover the same logic by injection; this one adds only fd 3 itself.
func TestActivation_ChildServesOnFD3(t *testing.T) {
	if os.Getenv("MEERKAT_TEST_EXEC") != "1" {
		t.Skip("re-executes the test binary; set MEERKAT_TEST_EXEC=1 to run")
	}
	tl, f := tcpListenerFile(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestActivationHelperProcess$")
	cmd.Env = append(os.Environ(), "MEERKAT_ACTIVATION_HELPER=1", "LISTEN_FDS=1", "LISTEN_FDNAMES=http")
	cmd.ExtraFiles = []*os.File{f} // ExtraFiles[0] is the child's fd 3
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + tl.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET through the child's inherited socket: %v\n%s", err, stderr.String())
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got := string(body); got != "activated; env cleared" {
		t.Fatalf("child answered %q\n%s", got, stderr.String())
	}
}

// TestActivationHelperProcess is the child half of
// TestActivation_ChildServesOnFD3; it does nothing unless that test
// started it.
func TestActivationHelperProcess(t *testing.T) {
	if os.Getenv("MEERKAT_ACTIVATION_HELPER") != "1" {
		t.Skip("helper process only")
	}
	_ = os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	ln, err := ActivationListener()
	if err != nil || ln == nil {
		fmt.Fprintf(os.Stderr, "ActivationListener = %v, %v\n", ln, err)
		os.Exit(2)
	}
	cleared := os.Getenv("LISTEN_PID") == "" && os.Getenv("LISTEN_FDS") == "" && os.Getenv("LISTEN_FDNAMES") == ""
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if cleared {
			_, _ = io.WriteString(w, "activated; env cleared")
		} else {
			_, _ = io.WriteString(w, "activated; env NOT cleared")
		}
		go func() { time.Sleep(100 * time.Millisecond); os.Exit(0) }()
	})}
	_ = srv.Serve(ln)
	os.Exit(0)
}
