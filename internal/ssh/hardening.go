package ssh

import (
	gossh "github.com/charmbracelet/ssh"
)

// Session hardening: defense in depth.
//
// hostthis sessions are short-lived single-command exchanges:
//   - exec (or shell with a PTY) → verb runs → exit
// Nothing about that requires port-forwarding, agent-forwarding, X11, or
// subsystem channels. Disabling them shrinks the blast radius if an
// attacker ever compromises an identity: the SSH connection becomes
// useless as a TCP-tunnel pivot, a credential-relay surface, or an
// SFTP/SCP file mover.
//
// Mechanism summary (charmbracelet/ssh, gliderlabs fork):
//   - LocalPortForwardingCallback returning false → refuses
//     `direct-tcpip` channel requests (ssh -L). The library's tcpip.go
//     denies when the callback is nil OR returns false; we set an
//     explicit `false` callback for clarity and to survive any future
//     upstream default flip.
//   - ReversePortForwardingCallback returning false → refuses
//     `tcpip-forward` requests (ssh -R). Same shape as above.
//   - SessionRequestCallback returning false for "subsystem" → refuses
//     SFTP/SCP-as-subsystem and any other named subsystem. The library
//     consults this callback on subsystem requests; "shell" and "exec"
//     are explicitly allowed so normal verb sessions still work.
//
// Already covered by upstream defaults:
//   - SubsystemHandlers is nil → unknown subsystems are refused with
//     a reply(false). The SessionRequestCallback gate is belt and
//     suspenders so a future upstream change that adds a default
//     subsystem doesn't quietly expose us.
//   - x11-req is not in the library's request switch → it falls to the
//     default case which replies(false). No additional gate needed,
//     but documented here so the contract is explicit.
//   - auth-agent-req@openssh.com is acknowledged by the library but no
//     forwarding socket is ever set up by hostthis (we never call into
//     ssh.AgentRequested), so an "approved" agent-req is a no-op for
//     any client that tried to use it.
//
// We keep PTY allocation enabled: the verb-help formatter switches LF →
// CRLF when a PTY is present, and the test suite exercises both shapes.
// PTY itself is not a tunnel; it's just stdin/stdout wrapped in line
// discipline.

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
