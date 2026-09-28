//go:build linux

package mcp

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// activation_linux.go is systemd socket activation for `mk mcp serve-http`
// (#110): serving on a socket systemd bound and passed in, instead of
// binding one.
//
// # Why
//
// A restart otherwise closes the port for as long as the new process
// takes to resolve content and build every index, and a client that
// connects in that window is refused. With a .socket unit, systemd owns
// the listening socket across restarts: a connection that arrives while
// the service restarts waits in the kernel's accept queue and is served
// once the new process calls Accept.

// listenFDsStart is SD_LISTEN_FDS_START: systemd passes the first socket
// as fd 3.
const listenFDsStart = 3

// ActivationListener returns the listening socket systemd passed this
// process, or nil when it passed none, in which case the caller binds
// its own. It follows sd_listen_fds(3):
//
//   - LISTEN_PID must name this process. Any other value means the
//     variables were inherited from a parent and describe ITS sockets, so
//     they are ignored.
//   - LISTEN_PID, LISTEN_FDS and LISTEN_FDNAMES are unset whenever they
//     were present, and the inherited fd is marked close-on-exec. A child
//     process (a git credential helper, an ingest run) inherits neither
//     the socket nor a claim to it.
//
// Exactly one socket is accepted. Choosing among several would need
// LISTEN_FDNAMES and a flag to name one, and a guess would serve on the
// wrong socket without saying so. So more than one is an error that
// names the count, and so is a socket that is not listening: that is
// what `Accept=yes` passes, one connected socket per client.
func ActivationListener() (net.Listener, error) {
	return activationEnv{
		getenv:   os.Getenv,
		unsetenv: os.Unsetenv,
		pid:      os.Getpid(),
		fd:       listenFDsStart,
	}.listener()
}

// activationEnv is ActivationListener's inputs, injectable so a test can
// pass a socket it owns rather than the process's real fd 3.
type activationEnv struct {
	getenv   func(string) string
	unsetenv func(string) error
	pid      int
	fd       int
}

func (e activationEnv) listener() (net.Listener, error) {
	pidVar, fdsVar := e.getenv("LISTEN_PID"), e.getenv("LISTEN_FDS")
	if pidVar == "" && fdsVar == "" {
		return nil, nil
	}
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		_ = e.unsetenv(k)
	}
	if pid, err := strconv.Atoi(pidVar); err != nil || pid != e.pid {
		return nil, nil // not ours: a parent's sockets, not this process's
	}
	if fdsVar == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(fdsVar)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("socket activation: LISTEN_FDS=%q is not a socket count", fdsVar)
	}
	switch {
	case n == 0:
		return nil, nil
	case n > 1:
		return nil, fmt.Errorf("socket activation: systemd passed %d sockets; serve-http takes exactly one (a single ListenStream= in the .socket unit)", n)
	}

	syscall.CloseOnExec(e.fd)
	if on, err := syscall.GetsockoptInt(e.fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN); err != nil || on == 0 {
		return nil, fmt.Errorf("socket activation: fd %d is not a listening socket; use ListenStream= with Accept=no (the default) in the .socket unit", e.fd)
	}
	f := os.NewFile(uintptr(e.fd), "LISTEN_FD_3")
	ln, err := net.FileListener(f)
	// FileListener works on a duplicate, so the original is closed either
	// way: one descriptor for the socket, not two.
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("socket activation: fd %d: %w", e.fd, err)
	}
	return ln, nil
}
