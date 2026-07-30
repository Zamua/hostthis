package ssh

import (
	gossh "github.com/charmbracelet/ssh"
)

// Session hardening: defence in depth.
//
// A hostthis session is a short-lived single-command exchange, which needs no
// port-forwarding, agent-forwarding, X11 or subsystem channels. Disabling them
// shrinks the blast radius of a compromised identity: the connection becomes
// useless as a TCP-tunnel pivot, a credential-relay surface, or an SFTP mover.
//
// Explicitly gated:
//
//   - LocalPortForwardingCallback false refuses direct-tcpip (ssh -L).
//   - ReversePortForwardingCallback false refuses tcpip-forward (ssh -R).
//   - SessionRequestCallback false for "subsystem" refuses SFTP/SCP and any
//     other named subsystem; "shell" and "exec" stay allowed.
//
// The library already denies the first two when the callback is nil, and
// already refuses unknown subsystems and x11-req by default. The explicit
// callbacks exist so an upstream default flip cannot quietly expose us.
//
// auth-agent-req is acknowledged by the library, but hostthis never sets up a
// forwarding socket, so an approved agent-req is a no-op.
//
// PTY allocation stays ENABLED: the help formatter switches LF to CRLF when a
// PTY is present. A PTY is line discipline, not a tunnel.

// withHardening returns an ssh.Option that disables port-forwarding,
// reverse port-forwarding, and subsystem requests on the wish server.
// The option is intended to be passed to wish.NewServer alongside the
// existing With* options.
func withHardening() gossh.Option {
	return func(srv *gossh.Server) error {
		srv.LocalPortForwardingCallback = func(_ gossh.Context, _ string, _ uint32) bool {
			return false
		}
		srv.ReversePortForwardingCallback = func(_ gossh.Context, _ string, _ uint32) bool {
			return false
		}
		// SessionRequestCallback is invoked for "shell", "exec", and
		// "subsystem" requests. Refuse subsystem outright; allow the
		// others so verb sessions (and the interactive `ssh <apex>` PTY
		// shell) keep working.
		srv.SessionRequestCallback = func(_ gossh.Session, requestType string) bool {
			return requestType != "subsystem"
		}
		return nil
	}
}
