// Package ssh implements the SSH-pipe surface — the user's full CLI.
// Every session must offer a publickey; sessions without one are
// rejected at startup. The presented key's SHA256 fingerprint becomes
// the identity passed to the application services for quota
// accounting and ownership checks.
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
	"strconv"
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
	KeyGate     *service.KeyGate // optional; nil disables the Sybil rate limit
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
// command means "implicit upload of whatever's on stdin."
func (s *Server) handleSession(sess gossh.Session) {
	// hostthis requires an ssh key on every session. Without a key
	// there's no identity to attribute the paste to or to enforce
	// the per-identity quota against. Bail with a helpful message.
	keyedFP, _ := sess.Context().Value("ownerHash").(string)
	if keyedFP == "" {
		fmt.Fprintln(sess.Stderr(), "hostthis: ssh key required.")
		fmt.Fprintln(sess.Stderr(), "  generate one (ssh-keygen -t ed25519) and add it to ssh-agent,")
		fmt.Fprintln(sess.Stderr(), "  or pass it on the command line: ssh -i ~/.ssh/id_ed25519 hostthis.dev")
		_ = sess.Exit(3)
		return
	}
	owner := domain.IdentityFromKeyFingerprint(keyedFP).String()

	// Sybil rate limit: cap the number of distinct fresh fingerprints
	// any one IP subnet can introduce in a 24h window. Returning users
	// (any (key, subnet) we've seen before) pass through with no
	// accounting.
	if s.KeyGate != nil {
		subnet := ipSubnet(remoteIP(sess))
		if err := s.KeyGate.Admit(owner, subnet); err != nil {
			if errors.Is(err, service.ErrSybilRateLimit) {
				fmt.Fprintln(sess.Stderr(), "hostthis: too many new keys from this network today.")
				fmt.Fprintln(sess.Stderr(), "  try again tomorrow, or use an existing key already known to hostthis.")
				_ = sess.Exit(6)
				return
			}
			fmt.Fprintf(sess.Stderr(), "hostthis: key gate: %v\n", err)
			_ = sess.Exit(1)
			return
		}
	}

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
	case "versions":
		s.verbVersions(sess, owner, argv[1:])
	case "pin":
		s.verbPin(sess, owner, argv[1:])
	case "unpin":
		s.verbUnpin(sess, owner, argv[1:])
	case "whoami":
		s.verbWhoami(sess, owner)
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
	//   nil / []              → new upload
	//   [<slug>]              → update an existing slug
	//   [--name "label"] etc. → new upload with flags
	//   [<slug> --name "…"]   → update with flags (rename in one shot)
	args, err := parseUploadFlags(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	// Read up to the raw-byte hard fast-fail. The compressed-size cap
	// is enforced by the service layer once the body is in hand.
	body, err := io.ReadAll(io.LimitReader(sess, int64(domain.HardRawByteCap)+1))
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: read upload: %v\n", err)
		_ = sess.Exit(1)
		return
	}
	if len(body) > domain.HardRawByteCap {
		fmt.Fprintf(sess.Stderr(), "hostthis: upload too large to consider (raw input exceeded %d-byte cap)\n", domain.HardRawByteCap)
		_ = sess.Exit(1)
		return
	}

	if args.Slug != "" {
		// Update path.
		slug, _ := domain.ParseSlug(args.Slug)
		res, err := s.Manage.Update(slug, owner, body, args.Type)
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
			_ = sess.Exit(exitForServiceErr(err))
			return
		}
		if args.Name != "" {
			_ = s.Manage.Rename(slug, owner, args.Name)
		}
		url := s.BuildURL(res.Paste.Slug)
		fmt.Fprintln(sess, url)
		fmt.Fprintf(sess.Stderr(), "v%d saved — expires in 7 days\n", res.NewVer)
		if res.WasPinned {
			fmt.Fprintf(sess.Stderr(),
				"note: this paste is pinned to v%d, so the URL still serves v%d, not v%d.\n",
				res.PinnedAt, res.PinnedAt, res.NewVer)
			fmt.Fprintf(sess.Stderr(), "  ssh hostthis.dev unpin %s        # always serve latest\n", slug)
			fmt.Fprintf(sess.Stderr(), "  ssh hostthis.dev pin %s %d       # serve this new version\n", slug, res.NewVer)
		}
		_ = sess.Exit(0)
		return
	}

	// Create path.
	res, err := s.Upload.Create(body, owner, args.Name, args.Type)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	url := s.BuildURL(res.Paste.Slug)
	fmt.Fprintln(sess, url)
	if res.Paste.Name != "" {
		fmt.Fprintf(sess.Stderr(), "%q — expires in 7 days\n", res.Paste.Name)
	} else {
		fmt.Fprintln(sess.Stderr(), "expires in 7 days")
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
	fmt.Fprintln(sess.Stderr(), "SLUG\tNAME\tSIZE\tKIND\tEXPIRES_IN\tVERS")
	now := s.Manage.Now().UTC()
	for _, p := range pastes {
		name := p.Name
		if name == "" {
			name = "—"
		}
		fmt.Fprintf(sess, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Slug, name, humanBytes(p.Size), p.Kind,
			humanDuration(p.ExpiresAt.Sub(now)), renderVersCol(p))
	}
	_ = sess.Exit(0)
}

// renderVersCol renders the VERS column for `list`. Three states per spec:
//
//	unpinned                       → "v<latest>"
//	pinned, pin == latest          → "v<latest> (pinned)"
//	pinned, pin <  latest          → "v<pin> (pinned, latest v<latest>)"
//
// LatestVersion comes from MAX(ver_num); always >= 1 for active pastes.
func renderVersCol(p domain.Paste) string {
	if p.PinnedVersion == 0 {
		return fmt.Sprintf("v%d", p.LatestVersion)
	}
	if p.PinnedVersion >= p.LatestVersion {
		return fmt.Sprintf("v%d (pinned)", p.PinnedVersion)
	}
	return fmt.Sprintf("v%d (pinned, latest v%d)", p.PinnedVersion, p.LatestVersion)
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
	// Two-shape dispatch:
	//   delete <slug>           → wipe the whole paste
	//   delete <slug> <verN>    → tombstone just that version
	// Anything else → usage error.
	switch len(argv) {
	case 0:
		fmt.Fprintln(sess.Stderr(), "usage: delete <slug> [<ver>]")
		_ = sess.Exit(2)
		return
	case 1:
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
		return
	case 2:
		slug, err := requireSlug(argv[:1])
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
			_ = sess.Exit(2)
			return
		}
		verNum, err := parseVersionArg(argv[1])
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
			_ = sess.Exit(2)
			return
		}
		res, err := s.Manage.DeleteVersion(slug, owner, verNum)
		switch {
		case err == nil:
			fmt.Fprintf(sess.Stderr(), "deleted v%d. freed %s.\n", res.VerNum, humanBytes(res.FreedBytes))
			_ = sess.Exit(0)
		case errors.Is(err, service.ErrVersionAlreadyDeleted):
			fmt.Fprintf(sess.Stderr(), "version v%d already deleted\n", verNum)
			_ = sess.Exit(0)
		case errors.Is(err, service.ErrVersionCurrentlyServed):
			fmt.Fprintf(sess.Stderr(), "hostthis: v%d is currently served — pin a different version first (or `unpin` if pinning was active)\n", verNum)
			_ = sess.Exit(2)
		default:
			emitServiceErr(sess, err)
		}
		return
	default:
		fmt.Fprintln(sess.Stderr(), "usage: delete <slug> [<ver>]")
		_ = sess.Exit(2)
		return
	}
}

// parseVersionArg parses "2" or "v2" → 2. Rejects negatives and zero.
func parseVersionArg(s string) (int, error) {
	s = strings.TrimPrefix(s, "v")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid version %q (want a positive integer like 2 or v2)", s)
	}
	return n, nil
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

	// `current` marker: when pinned_version is 0, the served version
	// is the MAX non-deleted ver_num. ListVersions returns newest first
	// (including tombstones), so we walk forward to the first non-deleted.
	servedVer := p.PinnedVersion
	if servedVer == 0 {
		for _, v := range vers {
			if !v.Deleted {
				servedVer = v.VerNum
				break
			}
		}
	}

	for _, v := range vers {
		marker := "       "
		switch {
		case v.Deleted:
			marker = "deleted"
		case v.VerNum == servedVer:
			marker = "current"
		}
		size := humanBytes(v.Size)
		if v.Deleted {
			size = "—"
		}
		fmt.Fprintf(sess, "v%d\t%s\t%s\t%s\n",
			v.VerNum, marker, v.CreatedAt.Format("2006-01-02 15:04 UTC"), size)
	}
	pinNote := "unpinned"
	if p.PinnedVersion != 0 {
		pinNote = fmt.Sprintf("pinned to v%d", p.PinnedVersion)
	}
	fmt.Fprintf(sess.Stderr(), "%s — expires in %s (%s)\n",
		pinNote, humanDuration(p.ExpiresAt.Sub(now)), p.ExpiresAt.Format("2006-01-02 15:04 UTC"))
	_ = sess.Exit(0)
}

func (s *Server) verbUnpin(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(2)
		return
	}
	if err := s.Manage.Unpin(slug, owner); err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintln(sess.Stderr(), "unpinned. URL now serves the latest version.")
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

// -- whoami -----------------------------------------------------------------

func (s *Server) verbWhoami(sess gossh.Session, owner string) {
	// handleSession rejects key-less sessions before they get here,
	// so owner is always a key:<fp> identity by this point.
	info, err := s.Manage.Whoami(owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	// info.Identity is "key:SHA256:abcd..." — strip the prefix for
	// display so it matches `ssh-keygen -lf` style.
	fmt.Fprintf(sess, "key:     %s\n", strings.TrimPrefix(info.Identity, domain.IdentityKeyPrefix))
	if !info.FirstSeen.IsZero() {
		fmt.Fprintf(sess, "joined:  %s\n", info.FirstSeen.Format("2006-01-02"))
	}
	fmt.Fprintf(sess, "active:  %d paste(s)\n", info.Active)
	_ = sess.Exit(0)
}

// -- help -------------------------------------------------------------------

func (s *Server) verbHelp(sess gossh.Session) {
	fmt.Fprintln(sess.Stderr(), helpText)
	_ = sess.Exit(0)
}

const helpText = `hostthis — pipe rendered content (html/markdown), get a URL.
              pastes expire 7 days after their last update.

  cat file | ssh hostthis.dev [--name "..."]      upload
  cat file | ssh hostthis.dev <slug>              update an existing upload
  ssh hostthis.dev list                           your active pastes
  ssh hostthis.dev show <slug>                    read content (owner only)
  ssh hostthis.dev rename <slug> "<name>"         set / change a paste's label
  ssh hostthis.dev versions <slug>                history within the 7-day window
  ssh hostthis.dev pin <slug> <ver>               stick the URL to <ver> (survives updates)
  ssh hostthis.dev unpin <slug>                   clear the pin; URL serves the latest
  ssh hostthis.dev delete <slug>                  permanent
  ssh hostthis.dev whoami                         your identity + active count

uploads accept HTML and Markdown only. 1 MiB per identity, total
across active pastes. 7-day retention.
the URL itself is the secret — 8-char random slug, ~10^12 possibilities.
share the URL with anyone you want; don't share it with anyone you don't.`

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

// remoteIP extracts the client's IP address from a session's
// RemoteAddr. Returns nil for unknown / unparseable.
func remoteIP(sess gossh.Session) net.IP {
	addr := sess.RemoteAddr()
	if addr == nil {
		return nil
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// ipSubnet returns the canonical subnet string for the Sybil gate.
// IPv4 → "/24" prefix; IPv6 → "/48". A nil IP becomes "unknown" so
// the gate treats it as one stable bucket rather than crashing.
func ipSubnet(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
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
