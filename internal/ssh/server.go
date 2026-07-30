// Package ssh implements the SSH-pipe CLI surface. Every session must offer a
// publickey; the key's SHA256 fingerprint is the identity the application
// services use for quota accounting and ownership checks.
package ssh

import (
	"bufio"
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
	"text/tabwriter"
	"time"

	gossh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	proxyproto "github.com/pires/go-proxyproto"
	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// Structured exit codes for SSH sessions, surfaced to the client as the ssh
// process exit code. The mapping is a public contract that scripts branch on,
// so a code is never reassigned; docs/SPEC.md "Exit codes" mirrors it.
//
// 5 is permanently unused: service.requireOwner collapses non-owner reads to
// ErrNotFound so existence cannot leak across identities, meaning the SSH
// surface never observes ErrNotOwner. A new code takes the next free slot.

// URLBuilder renders a slug as its public URL.
type URLBuilder func(domain.Slug) string

// PasteReader is the by-slug read the SSH layer needs beyond the verb service,
// used to mark which version `versions` currently serves. Depending on this
// rather than the verb service's repo keeps the SSH layer decoupled from how
// Manage is composed, in particular from the cache-invalidating decorator.
type PasteReader interface {
	Get(domain.Slug) (domain.Paste, error)
}

// SiteReader resolves a deployed static-site slug to its URL for `url` / `qr`.
// Optional: nil means those verbs resolve paste slugs only. Like PasteReader
// the lookup is not owner-scoped - the URL is a public capability, so resolving
// a live slug leaks nothing the URL doesn't.
type SiteReader interface {
	Get(domain.Slug) (domain.Site, error)
}

// Server is the SSH listener.
type Server struct {
	Addr        string
	HostKeyPath string
	ApexDomain  string // used in user-visible messages (help text, error hints).
	Upload      *service.Upload
	Deploy      *service.DeploySite // optional; nil disables static-site archive uploads
	Manage      service.PasteManager
	Pastes      PasteReader      // by-slug read for the `versions` current-marker
	Sites       SiteReader       // optional; by-slug site read for `url`/`qr` (nil = paste-only)
	KeyGate     *service.KeyGate // optional; nil disables the Sybil rate limit
	Now         func() time.Time // clock; defaults to time.Now when nil
	BuildURL    URLBuilder
	Logger      *log.Logger
}

// retention reads the content-TTL policy from the Upload service, its single
// source of truth. Falls back to the default when no upload service is wired.
func (s *Server) retention() domain.Retention {
	if s.Upload != nil {
		return s.Upload.Retention
	}
	return domain.DefaultRetention()
}

// expiresPhrase renders the retention policy for post-upload confirmations:
// "expires in 30 days", or "never expires" when retention is disabled.
func (s *Server) expiresPhrase() string {
	r := s.retention()
	if !r.Enabled() {
		return "never expires"
	}
	return "expires in " + r.Describe()
}

// now returns the injected clock, defaulting to time.Now.
func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ListenAndServe blocks, returning whatever the listener returns (nil after a
// clean shutdown or net.ErrClosed).
//
// HOSTTHIS_SSH_PROXY_PROTOCOL=true wraps the listener so PROXY protocol v1/v2
// headers from a TCP-level forwarder (traefik, haproxy, nginx stream) are
// parsed and RemoteAddr() carries the real client IP. Required whenever SSH
// sits behind a TCP router: without it every session appears to originate from
// the forwarder and the Sybil per-subnet limit collapses to a global cap.
func (s *Server) ListenAndServe() error {
	signer, err := s.hostSigner()
	if err != nil {
		return err
	}
	srv, err := wish.NewServer(
		wish.WithAddress(s.Addr),
		withHostSigner(signer),
		wish.WithPublicKeyAuth(func(ctx gossh.Context, key gossh.PublicKey) bool {
			ctx.SetValue("ownerHash", fingerprintKey(key))
			return true
		}),
		wish.WithPasswordAuth(func(_ gossh.Context, _ string) bool { return true }),
		wish.WithKeyboardInteractiveAuth(func(_ gossh.Context, _ xssh.KeyboardInteractiveChallenge) bool {
			return true
		}),
		// The LAST middleware in the list wraps the rest and runs FIRST, so
		// they are listed inner-to-outer to get the call order: keyRequired
		// (refuses keyless sessions), then the Sybil gate, then the dispatcher.
		wish.WithMiddleware(
			s.terminalMiddleware(),
			s.ratelimitMiddleware(),
			s.keyRequiredMiddleware(),
		),
		// Refuse port-forwarding, agent-forwarding, X11 and subsystem
		// (sftp/scp) channels: sessions are single-command exchanges that need
		// none of them. Rationale in hardening.go.
		withHardening(),
	)
	if err != nil {
		return fmt.Errorf("ssh wish server: %w", err)
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.Addr, err)
	}
	if strings.EqualFold(os.Getenv("HOSTTHIS_SSH_PROXY_PROTOCOL"), "true") {
		ln = &proxyproto.Listener{Listener: ln}
		s.Logger.Printf("ssh: PROXY protocol parsing enabled (real client IPs come from PROXY headers)")
	}
	s.Logger.Printf("ssh: listening on %s", s.Addr)
	err = srv.Serve(ln)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// hostSigner loads, or on first run creates, the PKCS8-PEM host key signer. A
// nil signer (empty HostKeyPath) makes wish.NewServer auto-generate an
// in-memory ed25519 key, which the test fixtures rely on.
func (s *Server) hostSigner() (xssh.Signer, error) {
	if s.HostKeyPath == "" {
		return nil, nil
	}
	signer, err := loadOrCreateHostKey(s.HostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh host key %q: %w", s.HostKeyPath, err)
	}
	return signer, nil
}

// withHostSigner registers a pre-loaded signer as a host key. A nil signer is
// skipped so wish's auto-generate path stays available.
func withHostSigner(signer xssh.Signer) gossh.Option {
	return func(srv *gossh.Server) error {
		if signer == nil {
			return nil
		}
		srv.AddHostKey(signer)
		return nil
	}
}

// keyRequiredMiddleware refuses sessions that authenticated without presenting
// a public key. Every session needs a key to attribute content to an identity
// and enforce the per-identity quota; keyless sessions exit ExitAuth.
func (s *Server) keyRequiredMiddleware() wish.Middleware {
	return func(next gossh.Handler) gossh.Handler {
		return func(sess gossh.Session) {
			keyedFP, _ := sess.Context().Value("ownerHash").(string)
			if keyedFP == "" {
				fmt.Fprintln(sess.Stderr(), "hostthis: ssh key required.")                                              //nolint:errcheck
				fmt.Fprintln(sess.Stderr(), "  generate one (ssh-keygen -t ed25519) and add it to ssh-agent,")          //nolint:errcheck
				fmt.Fprintf(sess.Stderr(), "  or pass it on the command line: ssh -i ~/.ssh/id_ed25519 %s\n", s.apex()) //nolint:errcheck
				_ = sess.Exit(ExitAuth)
				return
			}
			next(sess)
		}
	}
}

// ratelimitMiddleware enforces the Sybil cap on how many distinct fresh
// fingerprints one IP subnet may introduce per window. Any (key, subnet) pair
// already seen passes with no accounting. A nil KeyGate makes this a no-op.
func (s *Server) ratelimitMiddleware() wish.Middleware {
	return func(next gossh.Handler) gossh.Handler {
		return func(sess gossh.Session) {
			if s.KeyGate == nil {
				next(sess)
				return
			}
			keyedFP, _ := sess.Context().Value("ownerHash").(string)
			// keyRequired runs first, so this is unreachable; the guard keeps
			// the middleware correct if the chain is ever reordered.
			if keyedFP == "" {
				next(sess)
				return
			}
			owner := domain.IdentityFromKeyFingerprint(keyedFP).String()
			subnet := ipSubnet(remoteIP(sess))
			if err := s.KeyGate.Admit(owner, subnet); err != nil {
				if ref, ok := errors.AsType[*service.SybilRefusal](err); ok {
					fmt.Fprintf(sess.Stderr(), "hostthis: too many new keys from this network today\n")                                    //nolint:errcheck
					fmt.Fprintf(sess.Stderr(), "  subnet %s used %d of %d in the last 24h\n", ref.Subnet, ref.FreshCountInWindow, ref.Cap) //nolint:errcheck
					fmt.Fprintf(sess.Stderr(), "  your key %s isn't yet registered here\n", strings.TrimPrefix(owner, domain.IdentityKeyPrefix))
					fmt.Fprintln(sess.Stderr(), "to get in:") //nolint:errcheck
					fmt.Fprintln(sess.Stderr(), "  (a) use a key already known on this subnet")
					if frees := ref.NextSlotFreesAt(); !frees.IsZero() {
						fmt.Fprintf(sess.Stderr(), "  (b) wait until %s - the oldest entry ages out then\n", frees.UTC().Format("2006-01-02 15:04 UTC"))
					}
					_ = sess.Exit(ExitSybilRefuse)
					return
				}
				if errors.Is(err, service.ErrSybilRateLimit) {
					// Fallback when the rich enrichment failed.
					fmt.Fprintln(sess.Stderr(), "hostthis: too many new keys from this network today.")
					fmt.Fprintln(sess.Stderr(), "  try again tomorrow, or use an existing key already known to hostthis.")
					_ = sess.Exit(ExitSybilRefuse)
					return
				}
				fmt.Fprintf(sess.Stderr(), "hostthis: key gate: %v\n", err)
				_ = sess.Exit(ExitErr)
				return
			}
			next(sess)
		}
	}
}

// terminalMiddleware is the innermost middleware: the verb dispatcher. It
// ignores next (wish's noop stub) because nothing follows it in the chain.
func (s *Server) terminalMiddleware() wish.Middleware {
	return func(_ gossh.Handler) gossh.Handler {
		return s.handleSession
	}
}

// handleSession dispatches one ssh command. sess.Command() is the
// already-shell-split arg vector; its first token is the verb, and an empty
// command means an implicit upload of whatever is on stdin. Upstream
// middlewares guarantee the session carries a public key the gate admitted.
func (s *Server) handleSession(sess gossh.Session) {
	keyedFP, _ := sess.Context().Value("ownerHash").(string)
	owner := domain.IdentityFromKeyFingerprint(keyedFP).String()

	argv := sess.Command()

	if len(argv) == 0 {
		// A PTY means an interactive `ssh <apex>` with nothing piped in (a
		// pipe gets no PTY): show help rather than blocking on stdin.
		if _, _, hasPty := sess.Pty(); hasPty {
			s.verbHelp(sess, nil)
			return
		}
		s.verbUpload(sess, owner, nil)
		return
	}

	// A leading flag (e.g. `--name "foo"`) is an upload with no slug; without
	// this the dispatcher would treat the flag as a verb.
	if strings.HasPrefix(argv[0], "--") && argv[0] != "--help" {
		s.verbUpload(sess, owner, argv)
		return
	}

	// `<verb> --help` emits verb help instead of running the verb, so asking
	// for documentation never triggers a side effect (delete, pin, ...).
	if argvWantsHelp(argv) {
		if d, ok := lookupVerbDescriptor(argv[0]); ok {
			emitVerbHelp(sess, s.apex(), d)
			_ = sess.Exit(ExitOK)
			return
		}
	}

	switch first := argv[0]; first {
	case "help", "--help", "-h":
		s.verbHelp(sess, argv[1:])
	case "list":
		s.verbList(sess, owner, argv[1:])
	case "get":
		s.verbGet(sess, owner, argv[1:])
	case "url":
		s.verbURL(sess, argv[1:])
	case "qr":
		s.verbQR(sess, argv[1:])
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
		s.verbWhoami(sess, owner, argv[1:])
	default:
		// A bare slug is the update shortcut: `cat foo | ssh <apex> <slug>`.
		if _, err := domain.ParseSlug(first); err == nil {
			s.verbUpload(sess, owner, argv) // pass slug as first arg
			return
		}
		fmt.Fprintf(sess.Stderr(), "hostthis: unknown command %q\n\n", first)
		emitHelp(sess, s.apex(), s.retention())
		_ = sess.Exit(ExitUsage)
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
		_ = sess.Exit(ExitUsage)
		return
	}
	// The live session reader goes straight to the service layer, which streams
	// it through hash + compress + raw-counter (HardRawByteCap on the input
	// side, MaxPasteBytes on the compressed side). Buffering the body here
	// would peak memory at HardRawByteCap per concurrent upload.
	limited := io.LimitReader(sess, int64(domain.HardRawByteCap)+1)

	if args.Slug != "" {
		// Update path.
		slug, _ := domain.ParseSlug(args.Slug)

		// A gzip-tar archive piped at an owned site slug re-deploys in place.
		// The gzip magic decides site-vs-paste, the slug positional decides
		// new-vs-update. The peek is non-destructive: the buffered reader
		// replays the prefix downstream. Anything else is a paste update.
		if s.Deploy != nil && args.Type == "" {
			peeked := bufio.NewReaderSize(limited, 512)
			if head, _ := peeked.Peek(2); domain.HasGzipMagic(head) {
				s.deploySiteToSlug(sess, owner, slug, peeked)
				return
			}
			limited = peeked
		}

		res, err := s.Manage.Update(slug, owner, limited, args.Type)
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
		_, _ = fmt.Fprintf(sess.Stderr(), "v%d saved. %s\n", res.NewVer, s.expiresPhrase())
		if res.WasPinned {
			fmt.Fprintf(sess.Stderr(),
				"note: this paste is pinned to v%d, so the URL still serves v%d, not v%d.\n",
				res.PinnedAt, res.PinnedAt, res.NewVer)
			fmt.Fprintf(sess.Stderr(), "  ssh %s unpin %s        # always serve latest\n", s.apex(), slug)
			fmt.Fprintf(sess.Stderr(), "  ssh %s pin %s %d       # serve this new version\n", s.apex(), slug, res.NewVer)
		}
		writeQR(sess.Stderr(), url)
		_ = sess.Exit(ExitOK)
		return
	}

	// Create path. A gzip magic prefix (with no explicit type hint) routes to
	// the site deploy; everything else is a single-file paste. The peek is
	// non-destructive: the buffered reader replays the prefix downstream.
	peeked := bufio.NewReaderSize(limited, 512)
	if s.Deploy != nil && args.Type == "" {
		if head, _ := peeked.Peek(2); domain.HasGzipMagic(head) {
			s.deploySite(sess, owner, peeked)
			return
		}
	}

	res, err := s.Upload.Create(peeked, owner, args.Name, args.Type)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	url := s.BuildURL(res.Paste.Slug)
	fmt.Fprintln(sess, url)
	if res.Paste.Name != "" {
		_, _ = fmt.Fprintf(sess.Stderr(), "%q. %s\n", res.Paste.Name, s.expiresPhrase())
	} else {
		_, _ = fmt.Fprintln(sess.Stderr(), s.expiresPhrase())
	}
	writeQR(sess.Stderr(), url)
	_ = sess.Exit(ExitOK)
}

// deploySite runs the static-site archive path and returns the same shape of
// URL response a single-file upload does, so `tar czf - site/ | ssh <apex>`
// needs no verb and no flag.
func (s *Server) deploySite(sess gossh.Session, owner string, body io.Reader) {
	res, err := s.Deploy.Deploy(body, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	url := s.BuildURL(res.Site.Slug)
	fmt.Fprintln(sess, url)
	_, _ = fmt.Fprintf(sess.Stderr(), "site: %d file(s). %s\n", len(res.Site.Manifest.Files), s.expiresPhrase())
	writeQR(sess.Stderr(), url)
	_ = sess.Exit(ExitOK)
}

// deploySiteToSlug re-deploys a static-site archive at an existing owned slug,
// atomically swapping the site row; the slug and URL are unchanged. A slug
// naming a foreign-owned site, or not naming a site at all, exits with a
// not-found byte-for-byte identical to any other not-found, so a non-owner
// cannot probe existence or ownership. Sites have no name field, so --name is
// ignored here just as it is on the create path.
func (s *Server) deploySiteToSlug(sess gossh.Session, owner string, slug domain.Slug, body io.Reader) {
	res, err := s.Deploy.DeployToSlug(slug, body, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	url := s.BuildURL(res.Site.Slug)
	_, _ = fmt.Fprintln(sess, url)
	_, _ = fmt.Fprintf(sess.Stderr(), "site: %d file(s). %s\n", len(res.Site.Manifest.Files), s.expiresPhrase())
	writeQR(sess.Stderr(), url)
	_ = sess.Exit(ExitOK)
}

// -- list -------------------------------------------------------------------

func (s *Server) verbList(sess gossh.Session, owner string, argv []string) {
	format, _, err := parseOutputFormat(argv)
	if err != nil {
		emitUsageErr(sess, err)
		return
	}
	pastes, err := s.Manage.List(owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	// Sites count against the same quota as pastes but never expire, so `list`
	// must show them or the quota is invisible and unfreeable. A nil Deploy
	// means static-site hosting is disabled.
	var sites []domain.Site
	if s.Deploy != nil {
		sites, err = s.Deploy.ListSites(owner)
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
	}
	now := s.now().UTC()
	items := newListView(pastes, sites, now)

	if format == formatJSON {
		// json mode: stdout carries only the array (empty [] when nothing is
		// active), with no "no active pastes" stderr line.
		if err := writeJSON(sess, items); err != nil {
			emitServiceErr(sess, err)
			return
		}
		_ = sess.Exit(ExitOK)
		return
	}

	if len(items) == 0 {
		fmt.Fprintln(sess.Stderr(), "no active pastes")
		_ = sess.Exit(ExitOK)
		return
	}
	// The header goes on stdout, not stderr: stdout/stderr interleaving is
	// non-deterministic over ssh, so a stderr header can arrive after the rows.
	// tabwriter space-pads the columns so a long NAME cannot push every later
	// column out of alignment.
	tw := tabwriter.NewWriter(sess, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SLUG\tNAME\tSIZE\tKIND\tEXPIRES_IN\tVERS")
	for _, it := range items {
		name := it.Name
		if name == "" {
			name = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			it.Slug, name, humanBytes(it.SizeBytes), it.Kind,
			humanExpiresIn(it.expiresAt, now), versCol(it))
	}
	_ = tw.Flush()
	_ = sess.Exit(ExitOK)
}

// renderVersCol renders the VERS column for `list`. Three states:
//
//	unpinned                       → "v<latest>"
//	pinned, pin == latest          → "v<latest> (pinned)"
//	pinned, pin <  latest          → "v<pin> (pinned, latest v<latest>)"
//
// LatestVersion is MAX(ver_num), always >= 1 for an active paste.
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

func (s *Server) verbGet(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(ExitUsage)
		return
	}
	_, body, err := s.Manage.Show(slug, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	_, _ = sess.Write(body)
	_ = sess.Exit(ExitOK)
}

// -- url / qr ----------------------------------------------------------------

// verbURL prints the shareable URL for an existing slug on stdout. No
// ownership check (the URL is a public capability), but the target must exist
// and be unexpired, otherwise the standard not-found.
func (s *Server) verbURL(sess gossh.Session, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(ExitUsage)
		return
	}
	url, ok := s.resolveExistingURL(slug)
	if !ok {
		fmt.Fprintln(sess.Stderr(), "hostthis: not found")
		_ = sess.Exit(ExitNotFound)
		return
	}
	fmt.Fprintln(sess, url)
	_ = sess.Exit(ExitOK)
}

// verbQR mirrors create for an existing slug: the URL on stdout, the QR code
// on stderr. Same existence gate and not-found shape as verbURL.
func (s *Server) verbQR(sess gossh.Session, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(ExitUsage)
		return
	}
	url, ok := s.resolveExistingURL(slug)
	if !ok {
		fmt.Fprintln(sess.Stderr(), "hostthis: not found")
		_ = sess.Exit(ExitNotFound)
		return
	}
	fmt.Fprintln(sess, url)
	writeQR(sess.Stderr(), url)
	_ = sess.Exit(ExitOK)
}

// resolveExistingURL resolves slug to its shareable URL when it names a live
// (existing, unexpired) paste or, failing that, a live static site; false when
// no such slug exists. Reusing BuildURL keeps the result byte-identical to what
// the original upload returned. No ownership check: knowing the slug already
// grants read access at the URL.
func (s *Server) resolveExistingURL(slug domain.Slug) (string, bool) {
	now := s.now().UTC()
	if s.Pastes != nil {
		if p, err := s.Pastes.Get(slug); err == nil && now.Before(p.ExpiresAt) {
			return s.BuildURL(p.Slug), true
		}
	}
	if s.Sites != nil {
		if site, err := s.Sites.Get(slug); err == nil && now.Before(site.ExpiresAt) {
			return s.BuildURL(site.Slug), true
		}
	}
	return "", false
}

// -- rename ------------------------------------------------------------------

func (s *Server) verbRename(sess gossh.Session, owner string, argv []string) {
	if len(argv) < 1 {
		_, _ = fmt.Fprintln(sess.Stderr(), "hostthis: usage: rename <slug> [label]  (omit the label to clear it)")
		_ = sess.Exit(ExitUsage)
		return
	}
	slug, err := domain.ParseSlug(argv[0])
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: invalid slug %q\n", argv[0])
		_ = sess.Exit(ExitUsage)
		return
	}
	// ssh flattens the command to one space-joined string, so a multi-word
	// label arrives as several tokens and must be rejoined; quoting cannot
	// survive. No tokens clears the label, which is the only invocable clear
	// path since an empty-string argument cannot survive that join either.
	name := strings.Join(argv[1:], " ")
	if err := s.Manage.Rename(slug, owner, name); err != nil {
		emitServiceErr(sess, err)
		return
	}
	if name == "" {
		_, _ = fmt.Fprintln(sess.Stderr(), "label cleared.")
	} else {
		_, _ = fmt.Fprintln(sess.Stderr(), "renamed.")
	}
	_ = sess.Exit(ExitOK)
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
		_ = sess.Exit(ExitUsage)
		return
	case 1:
		slug, err := requireSlug(argv)
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
			_ = sess.Exit(ExitUsage)
			return
		}
		err = s.Manage.Delete(slug, owner)
		// A not-found paste may still name a site, so try the owner-checked
		// site delete before surfacing not-found. Only a clean site delete
		// short-circuits; any site-side error keeps the paste not-found so the
		// cases stay indistinguishable.
		if errors.Is(err, service.ErrNotFound) && s.Deploy != nil {
			if serr := s.Deploy.Delete(slug, owner); serr == nil {
				_, _ = fmt.Fprintln(sess.Stderr(), "deleted.")
				_ = sess.Exit(ExitOK)
				return
			}
		}
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
		fmt.Fprintln(sess.Stderr(), "deleted.")
		_ = sess.Exit(ExitOK)
		return
	case 2:
		slug, err := requireSlug(argv[:1])
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
			_ = sess.Exit(ExitUsage)
			return
		}
		verNum, err := parseVersionArg(argv[1])
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
			_ = sess.Exit(ExitUsage)
			return
		}
		res, err := s.Manage.DeleteVersion(slug, owner, verNum)
		switch {
		case err == nil:
			fmt.Fprintf(sess.Stderr(), "deleted v%d. freed %s.\n", res.VerNum, humanBytes(res.FreedBytes))
			_ = sess.Exit(ExitOK)
		case errors.Is(err, service.ErrVersionAlreadyDeleted):
			fmt.Fprintf(sess.Stderr(), "version v%d already deleted\n", verNum)
			_ = sess.Exit(ExitOK)
		case errors.Is(err, service.ErrVersionCurrentlyServed):
			fmt.Fprintf(sess.Stderr(), "hostthis: v%d is currently served. pin a different version first (or `unpin` if pinning was active)\n", verNum)
			_ = sess.Exit(ExitUsage)
		default:
			emitServiceErr(sess, err)
		}
		return
	default:
		fmt.Fprintln(sess.Stderr(), "usage: delete <slug> [<ver>]")
		_ = sess.Exit(ExitUsage)
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
	format, rest, err := parseOutputFormat(argv)
	if err != nil {
		emitUsageErr(sess, err)
		return
	}
	slug, err := requireSlug(rest)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(ExitUsage)
		return
	}
	vers, err := s.Manage.Versions(slug, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	// Guard a caller that left Pastes nil: the render still works, minus the
	// current-version marker.
	var p domain.Paste
	if s.Pastes != nil {
		p, _ = s.Pastes.Get(slug)
	}
	now := s.now().UTC()

	// With no pin, the served version is the highest non-deleted ver_num.
	// ListVersions is newest-first including tombstones, so the first
	// non-deleted entry is it.
	servedVer := p.PinnedVersion
	if servedVer == 0 {
		for _, v := range vers {
			if !v.Deleted {
				servedVer = v.VerNum
				break
			}
		}
	}

	if format == formatJSON {
		// json mode: stdout carries only the object; the pin/expiry footer
		// (normally on stderr) folds into the document.
		if err := writeJSON(sess, newVersionsView(string(slug), p, vers, servedVer, now)); err != nil {
			emitServiceErr(sess, err)
			return
		}
		_ = sess.Exit(ExitOK)
		return
	}

	// Space-padded columns (see verbList) so the marker column widens to fit
	// "current" / "deleted" and the date and size stay aligned.
	tw := tabwriter.NewWriter(sess, 0, 0, 2, ' ', 0)
	for _, v := range vers {
		marker := ""
		switch {
		case v.Deleted:
			marker = "deleted"
		case v.VerNum == servedVer:
			marker = "current"
		}
		size := humanBytes(v.Size)
		if v.Deleted {
			size = "-"
		}
		_, _ = fmt.Fprintf(tw, "v%d\t%s\t%s\t%s\n",
			v.VerNum, marker, v.CreatedAt.Format("2006-01-02 15:04 UTC"), size)
	}
	_ = tw.Flush()
	pinNote := "unpinned"
	if p.PinnedVersion != 0 {
		pinNote = fmt.Sprintf("pinned to v%d", p.PinnedVersion)
	}
	if p.ExpiresAt.Equal(domain.NeverExpires) {
		_, _ = fmt.Fprintf(sess.Stderr(), "%s. never expires\n", pinNote)
	} else {
		_, _ = fmt.Fprintf(sess.Stderr(), "%s. expires in %s (%s)\n",
			pinNote, humanDuration(p.ExpiresAt.Sub(now)), p.ExpiresAt.Format("2006-01-02 15:04 UTC"))
	}
	_ = sess.Exit(ExitOK)
}

func (s *Server) verbUnpin(sess gossh.Session, owner string, argv []string) {
	slug, err := requireSlug(argv)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
		_ = sess.Exit(ExitUsage)
		return
	}
	if err := s.Manage.Unpin(slug, owner); err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintln(sess.Stderr(), "unpinned. URL now serves the latest version.")
	_ = sess.Exit(ExitOK)
}

func (s *Server) verbPin(sess gossh.Session, owner string, argv []string) {
	if len(argv) < 2 {
		fmt.Fprintln(sess.Stderr(), "hostthis: usage: pin <slug> <ver-num>")
		_ = sess.Exit(ExitUsage)
		return
	}
	slug, err := domain.ParseSlug(argv[0])
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "hostthis: invalid slug %q\n", argv[0])
		_ = sess.Exit(ExitUsage)
		return
	}
	verStr := strings.TrimPrefix(argv[1], "v")
	verNum, err := parseInt(verStr)
	if err != nil || verNum < 1 {
		fmt.Fprintf(sess.Stderr(), "hostthis: invalid version %q\n", argv[1])
		_ = sess.Exit(ExitUsage)
		return
	}
	ver, err := s.Manage.Pin(slug, owner, verNum)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	fmt.Fprintf(sess.Stderr(), "pinned v%d.\n", ver.VerNum)
	_ = sess.Exit(ExitOK)
}

// -- whoami -----------------------------------------------------------------

func (s *Server) verbWhoami(sess gossh.Session, owner string, argv []string) {
	format, _, err := parseOutputFormat(argv)
	if err != nil {
		emitUsageErr(sess, err)
		return
	}
	// keyRequiredMiddleware ran first, so owner is always a key:<fp> identity.
	subnet := ipSubnet(remoteIP(sess))
	info, err := s.Manage.Whoami(owner, subnet)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}

	if format == formatJSON {
		if err := writeJSON(sess, newWhoamiView(info)); err != nil {
			emitServiceErr(sess, err)
			return
		}
		_ = sess.Exit(ExitOK)
		return
	}

	// info.Identity is "key:SHA256:abcd..."; stripping the prefix matches the
	// `ssh-keygen -lf` display style.
	fmt.Fprintf(sess, "key:     %s\n", strings.TrimPrefix(info.Identity, domain.IdentityKeyPrefix))
	if !info.FirstSeen.IsZero() {
		fmt.Fprintf(sess, "joined:  %s\n", info.FirstSeen.Format("2006-01-02"))
	}
	fmt.Fprintf(sess, "active:  %d paste(s)\n", info.Active)
	if info.QuotaBytes > 0 {
		pct := float64(info.UsedBytes) * 100 / float64(info.QuotaBytes)
		fmt.Fprintf(sess, "quota:   %s / %s (%.0f%%)\n",
			humanBytes(info.UsedBytes), humanBytes(info.QuotaBytes), pct)
	}
	if info.Session.Subnet != "" {
		fmt.Fprintln(sess)
		fmt.Fprintln(sess, "session:")
		fmt.Fprintf(sess, "  subnet:        %s\n", info.Session.Subnet)
		fmt.Fprintf(sess, "  seen subnets:  %d  (this one + %d other in the last 24h)\n",
			info.Session.IdentitySubnets, max0(info.Session.IdentitySubnets-1))
		fmt.Fprintf(sess, "  subnet budget: %d of %d fresh keys used here today\n",
			info.Session.SubnetFreshCount, info.Session.SubnetCap)
	}
	_ = sess.Exit(ExitOK)
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// -- help -------------------------------------------------------------------

// verbHelp dispatches help requests. No args emits the global banner, which
// characterization tests pin byte-exact. One arg naming a known verb (including
// doc-only aliases like `put` / `get`) emits verb help; anything else prefixes
// an `unknown verb` line and falls back to the banner, still exiting 0 because
// help is what was asked for.
func (s *Server) verbHelp(sess gossh.Session, rest []string) {
	if len(rest) == 0 {
		emitHelp(sess, s.apex(), s.retention())
		_ = sess.Exit(ExitOK)
		return
	}
	verb := rest[0]
	if d, ok := lookupVerbDescriptor(verb); ok {
		emitVerbHelp(sess, s.apex(), d)
		_ = sess.Exit(ExitOK)
		return
	}
	// PTY-aware CRLF for the prefix line, matching emitHelp's discipline.
	prefix := fmt.Sprintf("hostthis: unknown verb %q\n\n", verb)
	if _, _, hasPty := sess.Pty(); hasPty {
		prefix = strings.ReplaceAll(prefix, "\n", "\r\n")
	}
	fmt.Fprint(sess.Stderr(), prefix) //nolint:errcheck
	emitHelp(sess, s.apex(), s.retention())
	_ = sess.Exit(ExitOK)
}

// apex returns the configured apex domain. cmd/hostthisd refuses to start with
// an empty apex, so it is always non-empty in production; a test constructing
// Server directly must set ApexDomain.
func (s *Server) apex() string { return s.ApexDomain }

// emitHelp writes the help text to stderr, translating LF to CRLF when the
// session has a PTY. A client PTY is in raw mode and expects \r\n; a bare \n
// staircases the output (the cursor advances without returning to column 0).
// Interactive `ssh <apex>` allocates a PTY, `ssh <apex> help` does not.
func emitHelp(sess gossh.Session, apex string, retention domain.Retention) {
	text := helpText(apex, retention)
	if _, _, hasPty := sess.Pty(); hasPty {
		text = strings.ReplaceAll(text, "\n", "\r\n")
		fmt.Fprint(sess.Stderr(), text, "\r\n") //nolint:errcheck
		return
	}
	fmt.Fprintln(sess.Stderr(), text)
}

// helpTextTemplate is the canonical user-facing help. {{apex}} and
// {{retention}} are substituted at render time, so the text is correct under
// any deployment.
const helpTextTemplate = `Pipe a rendered file in, get a URL out. {{retention}}

UPLOAD  (-T silences the ssh pseudo-terminal warning on piped uploads;
         a QR code of the URL also prints to stderr on success)

    cat foo.html  | ssh -T {{apex}}
    cat doc.md    | ssh -T {{apex}} --name "design notes"
    git diff      | ssh -T {{apex}}                  rendered as a diff
    cat patch.txt | ssh -T {{apex}} --type diff      force the diff renderer

UPDATE & MANAGE (owner only; ssh key authenticates)

    cat foo.html | ssh -T {{apex}} <slug>   replace bytes; URL stays the same
    ssh {{apex}} list                       all your active pastes
    ssh {{apex}} get <slug>                 read content back
    ssh {{apex}} url <slug>                 re-show the URL (no QR)
    ssh {{apex}} qr <slug>                  re-show the URL + QR code
    ssh {{apex}} rename <slug> "label"      set / change owner label
    ssh {{apex}} delete <slug> [<ver>]      wipe the paste, or tombstone one version
    ssh {{apex}} whoami                     identity + active count + quota

VERSION HISTORY

    ssh {{apex}} versions <slug>            timeline of every version
    ssh {{apex}} pin <slug> <ver>           stick the URL to <ver> (survives updates)
    ssh {{apex}} unpin <slug>               URL follows latest again

OUTPUT

    list, versions, whoami accept -o json

STATIC SITES

    tar czf - site/ | ssh -T {{apex}}        deploy a multi-file site
    tar czf - site/ | ssh -T {{apex}} <slug> re-deploy in place

LIMITS

    100 MiB per identity, counting post-compression bytes across all
    your active pastes. HTML, Markdown, diff, or a gzip-tar site archive.

    Apps can persist + sync state: https://{{apex}}/  (rooms + realtime API)`

// helpText renders helpTextTemplate. apex must be non-empty.
func helpText(apex string, retention domain.Retention) string {
	t := strings.ReplaceAll(helpTextTemplate, "{{apex}}", apex)
	return strings.ReplaceAll(t, "{{retention}}", retentionSentence(retention))
}

// retentionSentence renders the help-text expiry line from the policy.
func retentionSentence(r domain.Retention) string {
	if !r.Enabled() {
		return "Pastes never expire."
	}
	return fmt.Sprintf("Pastes expire %s after last update.", r.Describe())
}

// -- helpers ----------------------------------------------------------------

func requireSlug(argv []string) (domain.Slug, error) {
	if len(argv) < 1 {
		return "", errors.New("missing slug")
	}
	return domain.ParseSlug(argv[0])
}

// emitUsageErr reports a bad-argument or parser failure on stderr and exits
// ExitUsage, the same code the dispatcher uses for a malformed command.
func emitUsageErr(sess gossh.Session, err error) {
	_, _ = fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
	_ = sess.Exit(ExitUsage)
}

func emitServiceErr(sess gossh.Session, err error) {
	switch {
	case errors.Is(err, service.ErrEmptyOwner):
		fmt.Fprintln(sess.Stderr(), "hostthis: add an ssh key. this command needs an identity")
	case errors.Is(err, service.ErrNotFound):
		fmt.Fprintln(sess.Stderr(), "hostthis: not found")
	case errors.Is(err, service.ErrInvalidName):
		fmt.Fprintln(sess.Stderr(), "hostthis: name must be 1–60 printable chars, no newlines")
	case errors.Is(err, domain.ErrUnsupportedKind):
		fmt.Fprintln(sess.Stderr(), "hostthis: "+domain.ErrUnsupportedKind.Error())
	case errors.Is(err, domain.ErrNoWebContent):
		fmt.Fprintln(sess.Stderr(), "hostthis: "+domain.ErrNoWebContent.Error())
	case errors.Is(err, domain.ErrUnsafeArchive):
		fmt.Fprintln(sess.Stderr(), "hostthis: "+domain.ErrUnsafeArchive.Error())
	case errors.Is(err, domain.ErrTooManyFiles):
		fmt.Fprintln(sess.Stderr(), "hostthis: "+domain.ErrTooManyFiles.Error())
	case errors.Is(err, service.ErrDeployFailed):
		// Show a clean retryable message, not the raw backend sentinel the
		// wrapped cause carries.
		_, _ = fmt.Fprintln(sess.Stderr(), "hostthis: site deploy failed, please retry")
	default:
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
	}
	_ = sess.Exit(exitForServiceErr(err))
}

// exitForServiceErr maps a service-layer error to its exit code. ErrNotOwner is
// deliberately absent: every owner-gated path in service.Manage collapses
// not-owner to ErrNotFound so existence cannot leak across identities, so a
// foreign-identity verb attempt and a missing slug both exit ExitNotFound.
func exitForServiceErr(err error) int {
	switch {
	case errors.Is(err, service.ErrEmptyOwner):
		return ExitAuth
	case errors.Is(err, service.ErrNotFound):
		return ExitNotFound
	default:
		return ExitErr
	}
}

// remoteIP extracts the client IP from a session's RemoteAddr, nil when
// unknown or unparseable.
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

// ipSubnet returns the canonical subnet string for the Sybil gate: IPv4 → /24,
// IPv6 → /48. A nil IP becomes "unknown" so the gate treats it as one stable
// bucket rather than crashing.
func ipSubnet(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

// fingerprintKey returns an ssh public key's SHA256 fingerprint, matching what
// `ssh-keygen -lf` emits.
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
