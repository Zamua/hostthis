package ssh

import (
	gossh "github.com/charmbracelet/ssh"
)

// Session hardening. A hostthis session is a short-lived single-command
// exchange, so port-forwarding (ssh -L / -R), X11 and subsystem channels
// (SFTP/SCP) are refused, leaving a compromised identity's connection useless
// as a tunnel pivot or file mover. The library already denies these on nil
// callbacks; the explicit callbacks exist so an upstream default flip cannot
// quietly re-expose them. auth-agent-req is acknowledged but no forwarding
// socket is ever set up, so it is a no-op.
//
// PTY allocation stays ENABLED: a PTY is line discipline, not a tunnel, and the
// help formatter needs it to pick CRLF.

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
