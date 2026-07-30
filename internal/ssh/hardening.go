package ssh

import (
	gossh "github.com/charmbracelet/ssh"
)

// Session hardening: defence in depth.
//
// A hostthis session is a short-lived single-command exchange, so
// port-forwarding, agent-forwarding, X11 and subsystem channels are all
// refused. That leaves a compromised identity's connection useless as a
// TCP-tunnel pivot, a credential-relay surface, or an SFTP mover:
//
//   - LocalPortForwardingCallback false refuses direct-tcpip (ssh -L).
//   - ReversePortForwardingCallback false refuses tcpip-forward (ssh -R).
//   - SessionRequestCallback false for "subsystem" refuses SFTP/SCP and any
//     other named subsystem; "shell" and "exec" stay allowed.
//
// The library already denies the first two on a nil callback and already
// refuses unknown subsystems and x11-req. The explicit callbacks exist so an
// upstream default flip cannot quietly re-expose them. auth-agent-req is
// acknowledged by the library, but no forwarding socket is ever set up, so an
// approved agent-req is a no-op.
//
// PTY allocation stays ENABLED: the help formatter switches LF to CRLF when a
// PTY is present. A PTY is line discipline, not a tunnel.

// withHardening returns the ssh.Option passed to wish.NewServer alongside the
// other With* options.
func withHardening() gossh.Option {
	return func(srv *gossh.Server) error {
		srv.LocalPortForwardingCallback = func(_ gossh.Context, _ string, _ uint32) bool {
			return false
		}
		srv.ReversePortForwardingCallback = func(_ gossh.Context, _ string, _ uint32) bool {
			return false
		}
		// Invoked for "shell", "exec" and "subsystem". Only the last is
		// refused; verb sessions and the interactive PTY shell need the others.
		srv.SessionRequestCallback = func(_ gossh.Session, requestType string) bool {
			return requestType != "subsystem"
		}
		return nil
	}
}
