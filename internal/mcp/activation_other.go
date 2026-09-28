//go:build !linux

package mcp

import "net"

// ActivationListener always returns nil off Linux: socket activation is
// systemd's protocol (activation_linux.go), and elsewhere serve-http
// binds --host/--port as it always has. The LISTEN_* variables are left
// alone, since nothing here reads them.
func ActivationListener() (net.Listener, error) { return nil, nil }
