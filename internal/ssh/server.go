// Package ssh implements the SSH-pipe surface — `cat file | ssh hostthis.dev`.
//
// The server accepts any auth (publickey or none) so anonymous uploads
// work. The user's public-key fingerprint, when present, is captured
// and passed to the application service as the owner identity. Without
// a key, the owner is empty string (anonymous).
//
// Verb dispatch reads the command the client sent (the bit after the
// host on the ssh CLI: `ssh hostthis.dev <verb> <args...>`). For Phase 1
// the only supported "verb" is the implicit upload — no args means
// "read stdin, upload, print URL." Other verbs return a not-yet
// stderr message.
package ssh

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

	gossh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// URLBuilder turns a slug into the URL we print on stdout. The
// production version is "https://<slug>.<apex>"; the dev/path mode
// is "https://<apex>/p/<slug>".
type URLBuilder func(domain.Slug) string

// Server is the SSH listener.
type Server struct {
	Addr     string         // e.g. ":2222"
	Upload   *service.Upload
	BuildURL URLBuilder
	Logger   *log.Logger
}

// ListenAndServe blocks. Returns whatever the listener returns —
// typically nil after a clean shutdown or net.ErrClosed.
func (s *Server) ListenAndServe() error {
	server := &gossh.Server{
		Addr: s.Addr,
		// Reject nothing. Public-key offered? capture the fingerprint.
		// Nothing offered (none auth)? still accept, fingerprint is "".
		PublicKeyHandler: func(ctx gossh.Context, key gossh.PublicKey) bool {
			ctx.SetValue("ownerHash", fingerprintKey(key))
			return true
		},
		// Allow connections with NO key. SSH's "none" method is
		// disabled by default in gliderlabs; flipping this on with
		// always-true PasswordHandler is the documented escape.
		PasswordHandler: func(_ gossh.Context, _ string) bool { return true },
		KeyboardInteractiveHandler: func(_ gossh.Context, _ xssh.KeyboardInteractiveChallenge) bool {
			return true
		},
		Handler: s.handleSession,
	}
	s.Logger.Printf("ssh: listening on %s", s.Addr)
	err := server.ListenAndServe()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// handleSession is invoked once per incoming session. We read the
// command, dispatch, and exit.
func (s *Server) handleSession(sess gossh.Session) {
	owner, _ := sess.Context().Value("ownerHash").(string)

	cmd := strings.TrimSpace(strings.Join(sess.Command(), " "))
	switch {
	case cmd == "":
		// Implicit upload. No verb, no args.
		s.handleUpload(sess, owner, "" /*name*/, "" /*typeHint*/)
	case isHelpRequest(cmd):
		s.handleHelp(sess)
	default:
		// Unknown verb in Phase 1. Tell the user, exit nonzero.
		fmt.Fprintf(sess.Stderr(), "hostthis: %q not implemented yet (Phase 1: upload only).\n", cmd)
		_ = sess.Exit(1)
	}
}

func (s *Server) handleUpload(sess gossh.Session, owner, name, typeHint string) {
	body, err := io.ReadAll(io.LimitReader(sess, int64(domain.MaxPasteBytes)+1))
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: read upload: %v\n", err)
		_ = sess.Exit(1)
		return
	}
	if len(body) > domain.MaxPasteBytes {
		fmt.Fprintf(sess.Stderr(), "hostthis: upload exceeds %d-byte cap\n", domain.MaxPasteBytes)
		_ = sess.Exit(1)
		return
	}
	res, err := s.Upload.Create(body, owner, name, typeHint)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(1)
		return
	}
	// URL on stdout (one line, pipe-friendly per SPEC.md).
	url := s.BuildURL(res.Paste.Slug)
	fmt.Fprintln(sess, url)
	// Note on stderr — user sees it in their terminal, pipes ignore it.
	fmt.Fprintf(sess.Stderr(), "expires in 24h\n")
	if owner == "" {
		fmt.Fprintf(sess.Stderr(), "note: anonymous upload — add an ssh key to get list/update/delete\n")
	}
	_ = sess.Exit(0)
}

func (s *Server) handleHelp(sess gossh.Session) {
	fmt.Fprintln(sess.Stderr(), "hostthis — pipe HTML or Markdown, get a URL")
	fmt.Fprintln(sess.Stderr(), "Phase 1 supports: anonymous upload only.")
	fmt.Fprintln(sess.Stderr(), "  cat file.html | ssh hostthis.dev")
	_ = sess.Exit(0)
}

func isHelpRequest(cmd string) bool {
	return cmd == "help" || cmd == "--help" || cmd == "-h"
}

// fingerprintKey returns the canonical SHA256 fingerprint of an ssh
// public key, prefixed with "SHA256:" so logs and `whoami` output
// match what `ssh-keygen -l` emits.
//
// gliderlabs hands us a generic ssh.PublicKey; we re-marshal it as
// the SSH wire form then sha256 it, matching the standard fingerprint
// algorithm.
func fingerprintKey(pk gossh.PublicKey) string {
	wire := pk.Marshal()
	sum := sha256.Sum256(wire)
	return "SHA256:" + hex.EncodeToString(sum[:])
}

