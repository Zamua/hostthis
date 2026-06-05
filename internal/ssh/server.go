// Package ssh implements the SSH-pipe surface — the user's full CLI.
// The server accepts any auth (publickey or none) so anonymous uploads
// work. The user's public-key fingerprint, when present, is captured
// and passed to the application services as the owner identity.
package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

	gossh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// URLBuilder turns a slug into the URL we print on stdout.
type URLBuilder func(domain.Slug) string

// Server is the SSH listener.
type Server struct {
	Addr        string
	HostKeyPath string
	Upload      *service.Upload
	Manage      *service.Manage
	Token       *service.TokenService
	BuildURL    URLBuilder
	Logger      *log.Logger
}

// ListenAndServe blocks. Returns whatever the listener returns —
// typically nil after a clean shutdown or net.ErrClosed.
func (s *Server) ListenAndServe() error {
	server := &gossh.Server{
		Addr: s.Addr,
		PublicKeyHandler: func(ctx gossh.Context, key gossh.PublicKey) bool {
			ctx.SetValue("ownerHash", fingerprintKey(key))
			return true
		},
		PasswordHandler: func(_ gossh.Context, _ string) bool { return true },
		KeyboardInteractiveHandler: func(_ gossh.Context, _ xssh.KeyboardInteractiveChallenge) bool {
			return true
		},
		Handler: s.handleSession,
	}
	if s.HostKeyPath != "" {
		signer, err := loadOrCreateHostKey(s.HostKeyPath)
		if err != nil {
			return fmt.Errorf("ssh host key %q: %w", s.HostKeyPath, err)
		}
		server.AddHostKey(signer)
	}
	s.Logger.Printf("ssh: listening on %s", s.Addr)
	err := server.ListenAndServe()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// handleSession dispatches one ssh command.
//
// `sess.Command()` returns the already-shell-split arg vector (ssh
// client does that for us). The first token is the verb. An empty
// command means "anonymous-or-keyed implicit upload."
func (s *Server) handleSession(sess gossh.Session) {
	owner, _ := sess.Context().Value("ownerHash").(string)
	argv := sess.Command()

	if len(argv) == 0 {
		// "ssh hostthis.dev" with nothing piped in: show help and exit.
		// We detect this by checking whether the client allocated a
		// PTY (interactive terminal) — pipes don't get a PTY. Without
		// this, we'd block reading stdin from a user just typing
		// `ssh hostthis.dev` to "see what it does."
		if _, _, hasPty := sess.Pty(); hasPty {
			s.verbHelp(sess)
			return
		}
		s.verbUpload(sess, owner, nil)
		return
	}

	// Flag in the first position (e.g. `--name "foo"`) means an upload
	// with no slug — flow into the upload path directly. Without this
	// the dispatcher tries to treat `--name` as a verb.
	if strings.HasPrefix(argv[0], "--") && argv[0] != "--help" {
		s.verbUpload(sess, owner, argv)
		return
	}

	switch first := argv[0]; first {
	case "help", "--help", "-h":
		s.verbHelp(sess)
	case "list":
		s.verbList(sess, owner)
	case "show":
		s.verbShow(sess, owner, argv[1:])
	case "rename":
		s.verbRename(sess, owner, argv[1:])
	case "delete":
		s.verbDelete(sess, owner, argv[1:])
	case "publish":
		s.verbPublish(sess, owner, argv[1:])
	case "unpublish":
		s.verbUnpublish(sess, owner, argv[1:])
	case "versions":
		s.verbVersions(sess, owner, argv[1:])
	case "pin":
		s.verbPin(sess, owner, argv[1:])
	case "link":
		s.verbLink(sess, owner, argv[1:])
	case "unshare":
		s.verbUnshare(sess, owner, argv[1:])
	case "whoami":
		s.verbWhoami(sess, owner)
	case "token":
		s.verbToken(sess, owner, argv[1:])
	default:
		// Looks like a slug? Treat as `update`. The slug-update
		// shortcut is the SPEC.md "cat foo | ssh hostthis.dev <slug>"
		// shape: no explicit verb, just the slug.
		if _, err := domain.ParseSlug(first); err == nil {
			s.verbUpload(sess, owner, argv) // pass slug as first arg
			return
		}
		// Unknown verb — print the error and the help, then exit nonzero.
		// Matches what git, kubectl, etc. do.
		fmt.Fprintf(sess.Stderr(), "hostthis: unknown command %q\n\n", first)
		fmt.Fprintln(sess.Stderr(), helpText)
		_ = sess.Exit(2)
	}
}

// -- upload (new + update) --------------------------------------------------

func (s *Server) verbUpload(sess gossh.Session, owner string, argv []string) {
	// argv may be:
	//   nil / []              → new anonymous-or-keyed upload
	//   [<slug>]              → update an existing slug
	//   [--name "label"] etc. → new upload with flags
	//   [<slug> --name "…"]   → update with flags (rename in one shot)
	args, err := parseUploadFlags(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
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

	if args.Slug != "" {
		// Update path.
		slug, _ := domain.ParseSlug(args.Slug)
		p, ver, err := s.Manage.Update(slug, owner, body, args.Type)
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
			_ = sess.Exit(exitForServiceErr(err))
			return
		}
		if args.Name != "" {
			_ = s.Manage.Rename(slug, owner, args.Name)
		}
		url := s.BuildURL(p.Slug)
		fmt.Fprintln(sess, url)
		fmt.Fprintf(sess.Stderr(), "v%d — expires in 24h\n", ver)
		_ = sess.Exit(0)
		return
	}

	// Create path.
	if owner == "" && args.Name != "" {
		fmt.Fprintln(sess.Stderr(), "note: --name ignored on anonymous upload")
		args.Name = ""
	}
	res, err := s.Upload.Create(body, owner, args.Name, args.Type)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(1)
		return
	}
	url := s.BuildURL(res.Paste.Slug)
	fmt.Fprintln(sess, url)
	if res.Paste.Name != "" {
		fmt.Fprintf(sess.Stderr(), "%q — expires in 24h\n", res.Paste.Name)
	} else {
		fmt.Fprintln(sess.Stderr(), "expires in 24h")
	}
	if owner == "" {
		fmt.Fprintln(sess.Stderr(), "note: anonymous upload — add an ssh key for list / update / delete")
	}
	_ = sess.Exit(0)
}

// -- list -------------------------------------------------------------------

func (s *Server) verbList(sess gossh.Session, owner string) {
	pastes, err := s.Manage.List(owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	if len(pastes) == 0 {
		fmt.Fprintln(sess.Stderr(), "no active pastes")
		_ = sess.Exit(0)
		return
	}
	// Header on stderr so stdout is grep/awk friendly.
	fmt.Fprintln(sess.Stderr(), "SLUG\tNAME\tSIZE\tKIND\tEXPIRES_IN\tVERS\tPUB")
	now := s.Manage.Now().UTC()
	for _, p := range pastes {
		name := p.Name
		if name == "" {
			name = "—"
		}
		pub := "y"
		if !p.Published {
			pub = "n"
		}
		fmt.Fprintf(sess, "%s\t%s\t%s\t%s\t%s\tv%d\t%s\n",
			p.Slug, name, humanBytes(p.Size), p.Kind,
			humanDuration(p.ExpiresAt.Sub(now)), p.PinnedVersion, pub)
	}
	_ = sess.Exit(0)
}

// -- show -------------------------------------------------------------------

func (s *Server) verbShow(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	_, body, err := s.Manage.Show(slug, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	_, _ = sess.Write(body)
	_ = sess.Exit(0)
}

// -- rename ------------------------------------------------------------------

func (s *Server) verbRename(sess gossh.Session, owner string, argv []string) {
	if len(argv) < 2 {
		fmt.Fprintln(sess.Stderr(), `hostthis: usage: rename <slug> "<name>"  (empty string clears)`)
		_ = sess.Exit(2)
		return
	}
	slug, err := domain.ParseSlug(argv[0])
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: invalid slug %q\n", argv[0])
		_ = sess.Exit(2)
		return
	}
	if err := s.Manage.Rename(slug, owner, argv[1]); err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintln(sess.Stderr(), "renamed.")
	_ = sess.Exit(0)
}

// -- delete -----------------------------------------------------------------

func (s *Server) verbDelete(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	if err := s.Manage.Delete(slug, owner); err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintln(sess.Stderr(), "deleted.")
	_ = sess.Exit(0)
}

// -- publish / unpublish ----------------------------------------------------

func (s *Server) verbPublish(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	if err := s.Manage.Publish(slug, owner); err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintln(sess.Stderr(), "published.")
	_ = sess.Exit(0)
}

func (s *Server) verbUnpublish(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	if err := s.Manage.Unpublish(slug, owner); err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintln(sess.Stderr(), "unpublished. URL now 404s for everyone but signed links.")
	_ = sess.Exit(0)
}

// -- versions / pin ---------------------------------------------------------

func (s *Server) verbVersions(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	vers, err := s.Manage.Versions(slug, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	p, _ := s.Manage.Repo.Get(slug)
	now := s.Manage.Now().UTC()
	for _, v := range vers {
		marker := "       "
		if v.VerNum == p.PinnedVersion {
			marker = "current"
		}
		fmt.Fprintf(sess, "v%d\t%s\t%s\t%s\n",
			v.VerNum, marker, v.CreatedAt.Format("2006-01-02 15:04 UTC"), humanBytes(v.Size))
	}
	fmt.Fprintf(sess.Stderr(), "expires in %s (%s)\n",
		humanDuration(p.ExpiresAt.Sub(now)), p.ExpiresAt.Format("2006-01-02 15:04 UTC"))
	_ = sess.Exit(0)
}

func (s *Server) verbPin(sess gossh.Session, owner string, argv []string) {
	if len(argv) < 2 {
		fmt.Fprintln(sess.Stderr(), "hostthis: usage: pin <slug> <ver-num>")
		_ = sess.Exit(2)
		return
	}
	slug, err := domain.ParseSlug(argv[0])
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: invalid slug %q\n", argv[0])
		_ = sess.Exit(2)
		return
	}
	verStr := strings.TrimPrefix(argv[1], "v")
	verNum, err := parseInt(verStr)
	if err != nil || verNum < 1 {
		fmt.Fprintf(sess.Stderr(), "hostthis: invalid version %q\n", argv[1])
		_ = sess.Exit(2)
		return
	}
	ver, err := s.Manage.Pin(slug, owner, verNum)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintf(sess.Stderr(), "pinned v%d.\n", ver.VerNum)
	_ = sess.Exit(0)
}

// -- link / unshare ---------------------------------------------------------

func (s *Server) verbLink(sess gossh.Session, owner string, argv []string) {
	if len(argv) < 1 {
		fmt.Fprintln(sess.Stderr(), "hostthis: usage: link <slug> [--expires <dur>]")
		_ = sess.Exit(2)
		return
	}
	slug, err := domain.ParseSlug(argv[0])
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: invalid slug %q\n", argv[0])
		_ = sess.Exit(2)
		return
	}
	lifetime, err := parseLifetimeFlag(argv[1:])
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	link, err := s.Manage.Link(slug, owner, lifetime)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintf(sess, "%s?k=%s\n", s.BuildURL(link.Slug), link.Token)
	fmt.Fprintf(sess.Stderr(), "expires %s\n", link.Expires.Format("2006-01-02 15:04 UTC"))
	_ = sess.Exit(0)
}

func (s *Server) verbUnshare(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	if err := s.Manage.Unshare(slug, owner); err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintln(sess.Stderr(), "all signed links revoked.")
	_ = sess.Exit(0)
}

// -- whoami -----------------------------------------------------------------

func (s *Server) verbWhoami(sess gossh.Session, owner string) {
	if owner == "" {
		fmt.Fprintln(sess.Stderr(), "anonymous — no ssh key offered")
		_ = sess.Exit(0)
		return
	}
	info, err := s.Manage.Whoami(owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintf(sess, "key:     %s\n", info.OwnerHash)
	if !info.FirstSeen.IsZero() {
		fmt.Fprintf(sess, "joined:  %s\n", info.FirstSeen.Format("2006-01-02"))
	}
	fmt.Fprintf(sess, "active:  %d paste(s)\n", info.Active)
	_ = sess.Exit(0)
}

// -- token ------------------------------------------------------------------

func (s *Server) verbToken(sess gossh.Session, owner string, argv []string) {
	if len(argv) < 1 {
		fmt.Fprintln(sess.Stderr(), "hostthis: usage: token create")
		_ = sess.Exit(2)
		return
	}
	switch argv[0] {
	case "create":
		raw, err := s.Token.Create(owner)
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
		fmt.Fprintln(sess, raw)
		fmt.Fprintln(sess.Stderr(), "save this — it's shown only once. use it as `Authorization: Bearer <token>` for the HTTP API.")
		_ = sess.Exit(0)
	default:
		fmt.Fprintf(sess.Stderr(), "hostthis: unknown token subcommand %q\n", argv[0])
		_ = sess.Exit(2)
	}
}

// -- help -------------------------------------------------------------------

func (s *Server) verbHelp(sess gossh.Session) {
	fmt.Fprintln(sess.Stderr(), helpText)
	_ = sess.Exit(0)
}

const helpText = `hostthis — pipe rendered content (html/markdown), get a URL.
              pastes expire 24h after their last update.

  cat file | ssh hostthis.dev [--name "..."]      upload
  cat file | ssh hostthis.dev <slug>              update an existing upload
  ssh hostthis.dev list                           your active pastes
  ssh hostthis.dev show <slug>                    read content (owner only)
  ssh hostthis.dev rename <slug> "<name>"         set / change a paste's label
  ssh hostthis.dev versions <slug>                history within the 24h window
  ssh hostthis.dev pin <slug> <ver>               set served version
  ssh hostthis.dev delete <slug>                  permanent
  ssh hostthis.dev unpublish <slug>               public 404s
  ssh hostthis.dev publish <slug>                 undo unpublish
  ssh hostthis.dev link <slug> [--expires <dur>]  signed share URL
  ssh hostthis.dev unshare <slug>                 revoke signed links
  ssh hostthis.dev whoami                         your identity + active count
  ssh hostthis.dev token create                   issue an HTTP API token

uploads accept HTML and Markdown only. 5 MB per paste. 24h retention.`

// -- helpers ----------------------------------------------------------------

func requireSlug(argv []string) (domain.Slug, error) {
	if len(argv) < 1 {
		return "", errors.New("missing slug")
	}
	return domain.ParseSlug(argv[0])
}

func emitServiceErr(sess gossh.Session, err error) {
	switch {
	case errors.Is(err, service.ErrEmptyOwner):
		fmt.Fprintln(sess.Stderr(), "hostthis: add an ssh key — this command needs an identity")
	case errors.Is(err, service.ErrNotFound):
		fmt.Fprintln(sess.Stderr(), "hostthis: not found")
	case errors.Is(err, service.ErrNotOwner):
		fmt.Fprintln(sess.Stderr(), "hostthis: not your paste")
	case errors.Is(err, service.ErrInvalidName):
		fmt.Fprintln(sess.Stderr(), "hostthis: name must be 1–60 printable chars, no newlines")
	case errors.Is(err, domain.ErrUnsupportedKind):
		fmt.Fprintln(sess.Stderr(), "hostthis: "+domain.ErrUnsupportedKind.Error())
	default:
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
	}
	_ = sess.Exit(exitForServiceErr(err))
}

func exitForServiceErr(err error) int {
	switch {
	case errors.Is(err, service.ErrEmptyOwner):
		return 3
	case errors.Is(err, service.ErrNotFound):
		return 4
	case errors.Is(err, service.ErrNotOwner):
		return 5
	default:
		return 1
	}
}

// fingerprintKey returns the canonical SHA256 fingerprint of an ssh
// public key, matching what `ssh-keygen -lf` emits.
func fingerprintKey(pk gossh.PublicKey) string {
	wire := pk.Marshal()
	sum := sha256.Sum256(wire)
	return "SHA256:" + hex.EncodeToString(sum[:])
}

func loadOrCreateHostKey(path string) (xssh.Signer, error) {
	if body, err := os.ReadFile(path); err == nil {
		signer, err := xssh.ParsePrivateKey(body)
		if err != nil {
			return nil, fmt.Errorf("parse existing host key: %w", err)
		}
		return signer, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read host key: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal host key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write host key: %w", err)
	}
	return xssh.ParsePrivateKey(pemBytes)
}
