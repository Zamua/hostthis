// Package ssh implements the SSH-pipe surface - the user's full CLI.
// Every session must offer a publickey; sessions without one are
// rejected at startup. The presented key's SHA256 fingerprint becomes
// the identity passed to the application services for quota
// accounting and ownership checks.
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
	"time"

	gossh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	proxyproto "github.com/pires/go-proxyproto"
	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// Structured exit codes for SSH sessions. The SSH protocol carries the
// exit status back to the local ssh client, which surfaces it as the
// process exit code; shell scripts and CI pipelines use these to branch.
// Keep the mapping stable: existing scripts will rely on specific codes.
//
// See docs/SPEC.md → "Exit codes" for the prose contract. The values
// here are the source of truth; the spec mirrors them.
//
// Note: 5 is intentionally unused. It was historically reserved for an
// ErrNotOwner path that the owner-collapse contract eliminated (the SSH
// surface never observes ErrNotOwner - service.requireOwner collapses
// non-owner reads to ErrNotFound so existence can't leak across
// identities). Do not reuse 5 for a new meaning; pick the next free
// slot to avoid retroactively changing the semantics of a code that
// was already documented.

// URLBuilder turns a slug into the URL we print on stdout.
type URLBuilder func(domain.Slug) string

// PasteReader is the narrow read access the SSH layer needs over and
// above the verb service - a single by-slug fetch used when rendering
// the `versions` output (to mark which version the URL currently
// serves). Depending on this interface, rather than reaching into the
// verb service's repo, keeps the SSH layer decoupled from how Manage is
// composed (in particular from the cache-invalidating decorator).
type PasteReader interface {
	Get(domain.Slug) (domain.Paste, error)
}

// SiteReader is the narrow by-slug read the SSH layer needs to resolve a
// deployed static-site slug to its URL for the `url` / `qr` verbs. It is
// optional: when nil, those verbs resolve paste slugs only. Like
// PasteReader, this is a non-owner-scoped lookup - the URL is a public
// capability, so resolving a live slug leaks nothing the URL doesn't.
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

// now returns the server's clock, defaulting to time.Now so tests that
// don't inject a clock still work.
func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ListenAndServe blocks. Returns whatever the listener returns -
// typically nil after a clean shutdown or net.ErrClosed.
//
// When HOSTTHIS_SSH_PROXY_PROTOCOL=true is set in the environment,
// the listener is wrapped with go-proxyproto's listener so PROXY
// protocol v1/v2 headers from a TCP-level forwarder (traefik, haproxy,
// nginx stream) are parsed and net.Conn.RemoteAddr() returns the real
// client IP. Required when hostthis SSH is behind traefik's TCP router
// - without it, every session looks like it's coming from the
// traefik container's docker-bridge IP and the Sybil per-subnet
// rate limit collapses to a global cap.
//
// The server is built with charmbracelet/wish: the underlying
// *ssh.Server is the charmbracelet fork of gliderlabs/ssh and the
// behavior matches the previous gliderlabs setup byte-for-byte. The
// session-time concerns (key-required, Sybil gate, verb dispatch) are
// expressed as wish middlewares stacked so the key-required check is
// outermost, the Sybil gate next, and the verb dispatcher innermost.
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
		// Middlewares compose first-to-last → the LAST middleware in the
		// argument list wraps the rest and runs FIRST per request. To
		// get the desired call order
		//   1. keyRequired   (outermost; refuses keyless sessions)
		//   2. ratelimit     (Sybil gate; refuses bursts of fresh keys)
		//   3. terminal      (the verb dispatcher)
		// we list them inner-to-outer.
		wish.WithMiddleware(
			s.terminalMiddleware(),
			s.ratelimitMiddleware(),
			s.keyRequiredMiddleware(),
		),
		// Defense-in-depth: refuse port-forwarding (-L / -R), agent-
		// forwarding usefulness, X11, and subsystem (sftp/scp) channels.
		// See hardening.go for the full rationale and the upstream-
		// defaults audit. hostthis sessions are short-lived
		// single-command exchanges; none of those channels are needed.
		withHardening(),
	)
	if err != nil {
		return fmt.Errorf("ssh wish server: %w", err)
	}
	// Plain Listen first; optionally wrap with go-proxyproto.
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

// hostSigner loads (or, on first run, creates) the host key signer
// using the existing PKCS8-PEM format. Returns nil signer when
// HostKeyPath is empty; wish.NewServer then auto-generates an
// in-memory ed25519 key, which is what the test fixtures rely on.
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

// withHostSigner returns an ssh.Option that registers a pre-loaded
// signer as a host key. Skips when signer is nil (so wish's auto-gen
// path stays available for tests).
func withHostSigner(signer xssh.Signer) gossh.Option {
	return func(srv *gossh.Server) error {
		if signer == nil {
			return nil
		}
		srv.AddHostKey(signer)
		return nil
	}
}

// keyRequiredMiddleware refuses sessions that authenticated via
// password / keyboard-interactive (no public key presented). hostthis
// needs a key on every session to attribute the paste to an identity
// and to enforce the per-identity quota. Without one we exit 3 with a
// helpful message pointing at ssh-keygen.
func (s *Server) keyRequiredMiddleware() wish.Middleware {
	return func(next gossh.Handler) gossh.Handler {
		return func(sess gossh.Session) {
			keyedFP, _ := sess.Context().Value("ownerHash").(string)
			if keyedFP == "" {
				fmt.Fprintln(sess.Stderr(), "hostthis: ssh key required.")
				fmt.Fprintln(sess.Stderr(), "  generate one (ssh-keygen -t ed25519) and add it to ssh-agent,")
				fmt.Fprintf(sess.Stderr(), "  or pass it on the command line: ssh -i ~/.ssh/id_ed25519 %s\n", s.apex())
				_ = sess.Exit(ExitAuth)
				return
			}
			next(sess)
		}
	}
}

// ratelimitMiddleware enforces the Sybil per-subnet cap on the number
// of distinct fresh fingerprints any one IP subnet can introduce in
// the configured window. Returning users - any (key, subnet) we've
// seen before - pass through with no accounting. When the gate is
// nil, the middleware is a no-op.
func (s *Server) ratelimitMiddleware() wish.Middleware {
	return func(next gossh.Handler) gossh.Handler {
		return func(sess gossh.Session) {
			if s.KeyGate == nil {
				next(sess)
				return
			}
			keyedFP, _ := sess.Context().Value("ownerHash").(string)
			// keyRequired runs before us, so a missing key would have
			// already exited. The defensive guard keeps the middleware
			// honest if the chain is reordered in a future refactor.
			if keyedFP == "" {
				next(sess)
				return
			}
			owner := domain.IdentityFromKeyFingerprint(keyedFP).String()
			subnet := ipSubnet(remoteIP(sess))
			if err := s.KeyGate.Admit(owner, subnet); err != nil {
				if ref, ok := errors.AsType[*service.SybilRefusal](err); ok {
					fmt.Fprintf(sess.Stderr(), "hostthis: too many new keys from this network today\n")
					fmt.Fprintf(sess.Stderr(), "  subnet %s used %d of %d in the last 24h\n", ref.Subnet, ref.FreshCountInWindow, ref.Cap)
					fmt.Fprintf(sess.Stderr(), "  your key %s isn't yet registered here\n", strings.TrimPrefix(owner, domain.IdentityKeyPrefix))
					fmt.Fprintln(sess.Stderr(), "to get in:")
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

// terminalMiddleware is the innermost middleware: the verb dispatcher.
// It ignores `next` (which is wish's noop stub) because the dispatcher
// is the terminal handler - there's nothing further in the chain.
func (s *Server) terminalMiddleware() wish.Middleware {
	return func(_ gossh.Handler) gossh.Handler {
		return s.handleSession
	}
}

// handleSession dispatches one ssh command.
//
// `sess.Command()` returns the already-shell-split arg vector (ssh
// client does that for us). The first token is the verb. An empty
// command means "implicit upload of whatever's on stdin." Key-required
// and Sybil-gate refusals are handled in upstream middlewares - by
// the time we get here we know the session has a public key and the
// gate (if configured) admitted it.
func (s *Server) handleSession(sess gossh.Session) {
	keyedFP, _ := sess.Context().Value("ownerHash").(string)
	owner := domain.IdentityFromKeyFingerprint(keyedFP).String()

	argv := sess.Command()

	if len(argv) == 0 {
		// "ssh hostthis.dev" with nothing piped in: show help and exit.
		// We detect this by checking whether the client allocated a
		// PTY (interactive terminal) - pipes don't get a PTY. Without
		// this, we'd block reading stdin from a user just typing
		// `ssh hostthis.dev` to "see what it does."
		if _, _, hasPty := sess.Pty(); hasPty {
			s.verbHelp(sess, nil)
			return
		}
		s.verbUpload(sess, owner, nil)
		return
	}

	// Flag in the first position (e.g. `--name "foo"`) means an upload
	// with no slug - flow into the upload path directly. Without this
	// the dispatcher tries to treat `--name` as a verb.
	if strings.HasPrefix(argv[0], "--") && argv[0] != "--help" {
		s.verbUpload(sess, owner, argv)
		return
	}

	// `<verb> --help` / `<verb> -h` intercept: if the first token is a
	// known verb (or a doc-only alias like `put` / `get`) and a help
	// flag appears later in argv, emit verb-specific help instead of
	// running the verb. This guards against side effects (delete, pin,
	// etc.) when the user only wanted documentation.
	if argvWantsHelp(argv) {
		if d, ok := lookupVerbDescriptor(argv[0]); ok {
			emitVerbHelp(sess, s.apex(), d)
			_ = sess.Exit(ExitOK)
			return
		}
	}

	switch first := argv[0]; first {
	case "help", "--help", "-h":
		// `help <verb>` → verb-specific help when <verb> is recognized;
		// otherwise an `unknown verb` prefix + the global banner. The
		// bare `help` (and the `--help` / `-h` no-verb forms) keep
		// emitting the global help banner; that path is pinned
		// byte-exact by the Phase A characterization tests.
		s.verbHelp(sess, argv[1:])
	case "list":
		s.verbList(sess, owner)
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
		s.verbWhoami(sess, owner)
	default:
		// Looks like a slug? Treat as `update`. The slug-update
		// shortcut is the SPEC.md "cat foo | ssh hostthis.dev <slug>"
		// shape: no explicit verb, just the slug.
		if _, err := domain.ParseSlug(first); err == nil {
			s.verbUpload(sess, owner, argv) // pass slug as first arg
			return
		}
		// Unknown verb - print the error and the help, then exit nonzero.
		// Matches what git, kubectl, etc. do.
		fmt.Fprintf(sess.Stderr(), "hostthis: unknown command %q\n\n", first)
		emitHelp(sess, s.apex())
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
	// Hand the live session reader to the service layer. The service
	// streams bytes through hash + compress + raw-counter, capping
	// at HardRawByteCap on the input side and MaxPasteBytes on the
	// compressed side. We DO NOT buffer the body here - that would
	// peak memory at HardRawByteCap per concurrent upload, which
	// blows the VPS RAM budget at modest concurrency.
	limited := io.LimitReader(sess, int64(domain.HardRawByteCap)+1)

	if args.Slug != "" {
		// Update path.
		slug, _ := domain.ParseSlug(args.Slug)

		// A gzip-tar archive piped to an OWNED site slug re-deploys that
		// site in place (same slug, same URL, new manifest), the static-
		// site analogue of a paste update. The format gate (gzip magic)
		// decides site-vs-paste exactly as the create path does; the slug
		// positional decides new-vs-update. Peek non-destructively - the
		// buffered reader replays the prefix downstream - and only when no
		// explicit text type was forced. Anything else falls through to the
		// paste-update path UNCHANGED.
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
		_, _ = fmt.Fprintf(sess.Stderr(), "v%d saved. expires in 30 days\n", res.NewVer)
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

	// Create path. Peek the leading bytes to detect a gzip-tar static-
	// site archive the same way the format gate detects HTML vs Markdown.
	// A gzip magic prefix (and no explicit text type hint) routes to the
	// site-deploy path; everything else is a single-file paste. The peek
	// is non-destructive: the buffered reader replays the prefix to the
	// downstream service.
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
		_, _ = fmt.Fprintf(sess.Stderr(), "%q. expires in 30 days\n", res.Paste.Name)
	} else {
		_, _ = fmt.Fprintln(sess.Stderr(), "expires in 30 days")
	}
	writeQR(sess.Stderr(), url)
	_ = sess.Exit(ExitOK)
}

// deploySite runs the static-site archive path: safe-untar the gzip-tar
// stream, store each file as a blob, build the manifest, persist the
// Site. Returns the same shape of URL response a single-file upload
// does - no new verb, no extra flags - so `tar czf - site/ | ssh apex`
// just works.
func (s *Server) deploySite(sess gossh.Session, owner string, body io.Reader) {
	res, err := s.Deploy.Deploy(body, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	url := s.BuildURL(res.Site.Slug)
	fmt.Fprintln(sess, url)
	_, _ = fmt.Fprintf(sess.Stderr(), "site: %d file(s). expires in 30 days\n", len(res.Site.Manifest.Files))
	writeQR(sess.Stderr(), url)
	_ = sess.Exit(ExitOK)
}

// deploySiteToSlug re-deploys a static-site archive at an existing OWNED
// slug in place: safe-untar the gzip-tar stream, store each file as a
// blob, build the manifest, and atomically swap the site row at slug. The
// slug and URL are unchanged; the same URL serves the new content. A slug
// that names a foreign-owned site, or that is not a site at all, maps
// through emitServiceErr to a not-found exit - byte-for-byte the same
// shape as any not-found, so a non-owner can't probe existence/ownership.
// Sites have no name field, so a --name flag (if any) is ignored here, the
// same as the create-path deploySite.
func (s *Server) deploySiteToSlug(sess gossh.Session, owner string, slug domain.Slug, body io.Reader) {
	res, err := s.Deploy.DeployToSlug(slug, body, owner)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	url := s.BuildURL(res.Site.Slug)
	_, _ = fmt.Fprintln(sess, url)
	_, _ = fmt.Fprintf(sess.Stderr(), "site: %d file(s). expires in 30 days\n", len(res.Site.Manifest.Files))
	writeQR(sess.Stderr(), url)
	_ = sess.Exit(ExitOK)
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
		_ = sess.Exit(ExitOK)
		return
	}
	// Header on stdout (was stderr historically - stderr ordering
	// vs stdout is non-deterministic over ssh, so the header could
	// arrive AFTER the rows from the user's perspective. The spec
	// shows the header at the top of the output; stdout + write-order
	// guarantees that). Scripts that want headerless output can
	// strip the first line with `tail -n +2`.
	fmt.Fprintln(sess, "SLUG\tNAME\tSIZE\tKIND\tEXPIRES_IN\tVERS")
	now := s.now().UTC()
	for _, p := range pastes {
		name := p.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(sess, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Slug, name, humanBytes(p.Size), p.Kind,
			humanDuration(p.ExpiresAt.Sub(now)), renderVersCol(p))
	}
	_ = sess.Exit(ExitOK)
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

// verbURL prints just the shareable URL for an existing slug on stdout.
// No ownership check - the URL is a public capability - but the target
// must exist and not be expired, otherwise the standard not-found
// (exit 4) is returned, the same shape as every other missing slug.
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

// verbQR mirrors create for an existing slug: the URL on stdout, the QR
// code on stderr. Same existence/expiry gate and not-found shape as
// verbURL; the only difference is the QR render.
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

// resolveExistingURL resolves slug to its shareable URL when the slug
// names a live (existing and non-expired) paste or, failing that, a live
// static site. The bool is false when no such live slug exists; the
// caller maps that to the standard not-found. URL construction is reused
// from BuildURL (the same logic the create path uses) so the result is
// byte-identical to what the original upload returned. No ownership
// check is performed: knowing the slug already grants read access at the
// URL, so there is nothing to leak that the URL doesn't already expose.
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
	// Join the remaining tokens as the label: ssh flattens the command to a
	// single space-joined string, so a multi-word label arrives as several
	// argv tokens and must be rejoined (quoting can't survive). No tokens at
	// all clears the label - the invocable clear path, since an empty-string
	// argument cannot survive the ssh argv-join.
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
		// A slug that is not a paste collapses to not-found, but it may name
		// a SITE. Fall through to the owner-checked site delete before
		// surfacing not-found, so `delete <slug>` takes a site down too. Only
		// a clean site delete short-circuits; any site-side error keeps the
		// original paste not-found so the cases stay indistinguishable.
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
	slug, err := requireSlug(argv)
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
	// Pastes is wired in production (and in any test that asserts the
	// current-version marker). Guard so a versions render still works -
	// minus the pinned-current marker - if a caller didn't supply it,
	// rather than nil-derefing.
	var p domain.Paste
	if s.Pastes != nil {
		p, _ = s.Pastes.Get(slug)
	}
	now := s.now().UTC()

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
			size = "-"
		}
		fmt.Fprintf(sess, "v%d\t%s\t%s\t%s\n",
			v.VerNum, marker, v.CreatedAt.Format("2006-01-02 15:04 UTC"), size)
	}
	pinNote := "unpinned"
	if p.PinnedVersion != 0 {
		pinNote = fmt.Sprintf("pinned to v%d", p.PinnedVersion)
	}
	fmt.Fprintf(sess.Stderr(), "%s. expires in %s (%s)\n",
		pinNote, humanDuration(p.ExpiresAt.Sub(now)), p.ExpiresAt.Format("2006-01-02 15:04 UTC"))
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

func (s *Server) verbWhoami(sess gossh.Session, owner string) {
	// keyRequiredMiddleware rejects key-less sessions before they get
	// here, so owner is always a key:<fp> identity by this point.
	subnet := ipSubnet(remoteIP(sess))
	info, err := s.Manage.Whoami(owner, subnet)
	if err != nil {
		emitServiceErr(sess, err)
		return
	}
	// info.Identity is "key:SHA256:abcd..." - strip the prefix for
	// display so it matches `ssh-keygen -lf` style.
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

// verbHelp dispatches help requests. With no extra args it emits the
// global help banner (Phase A pins this byte-exact). With one extra
// arg that matches a known verb (or doc-only alias like `put` / `get`),
// it emits verb-specific help. With one extra arg that doesn't match,
// it prefixes an `unknown verb` line and falls back to the global
// banner - mirroring the "unknown command" treatment for dispatch
// misses, but without the exit 2 (the user explicitly asked for help,
// so we hand them help and exit 0).
func (s *Server) verbHelp(sess gossh.Session, rest []string) {
	if len(rest) == 0 {
		emitHelp(sess, s.apex())
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
	fmt.Fprint(sess.Stderr(), prefix)
	emitHelp(sess, s.apex())
	_ = sess.Exit(ExitOK)
}

// apex returns the configured apex domain. The binary refuses to
// start with an empty apex (cmd/hostthisd enforces this), so this
// is always non-empty in production. Tests that construct Server
// directly must set ApexDomain too.
func (s *Server) apex() string { return s.ApexDomain }

// emitHelp writes the rendered help text to stderr, translating LF
// to CRLF when the session has a PTY allocated. The PTY is in raw
// mode on the client (it expects \r\n from the remote) and a bare
// \n produces a "staircase" effect - the cursor advances a line but
// doesn't return to column 0, so subsequent lines start where the
// previous one ended. An interactive `ssh <apex>` (no command)
// defaults to allocating a PTY; `ssh <apex> help` doesn't. Same
// helpText, different newline handling.
func emitHelp(sess gossh.Session, apex string) {
	text := helpText(apex)
	if _, _, hasPty := sess.Pty(); hasPty {
		text = strings.ReplaceAll(text, "\n", "\r\n")
		fmt.Fprint(sess.Stderr(), text, "\r\n")
		return
	}
	fmt.Fprintln(sess.Stderr(), text)
}

// helpTextTemplate is the canonical user-facing help. {{apex}}
// placeholders are substituted at render time with the configured
// apex domain so the help is correct under any deployment.
const helpTextTemplate = `Pipe a rendered file in, get a URL out. Pastes expire 30 days after last update.

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

STATIC SITES

    tar czf - site/ | ssh -T {{apex}}        deploy a multi-file site
    tar czf - site/ | ssh -T {{apex}} <slug> re-deploy in place

LIMITS

    10 MiB per identity, counting post-compression bytes across all
    your active pastes. HTML, Markdown, diff, or a gzip-tar site archive.

    Apps can persist + sync state: https://{{apex}}/  (rooms + realtime API)`

// helpText returns the rendered help with apex substituted in.
// Caller must pass a non-empty apex.
func helpText(apex string) string {
	return strings.ReplaceAll(helpTextTemplate, "{{apex}}", apex)
}

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
		// A site deploy hit an unexpected backend error (defensively translated,
		// e.g. a cross-shard bind that the slug pre-claim is designed to prevent).
		// Show the user a clean retryable message, NOT the raw backend sentinel
		// the wrapped cause carries.
		_, _ = fmt.Fprintln(sess.Stderr(), "hostthis: site deploy failed, please retry")
	default:
		fmt.Fprintf(sess.Stderr(), "hostthis: %v\n", err)
	}
	_ = sess.Exit(exitForServiceErr(err))
}

// exitForServiceErr maps a service-layer error to the canonical exit code.
// Note: ErrNotOwner is intentionally NOT a case here. Every owner-gated
// path in service.Manage collapses not-owner to ErrNotFound (see
// requireOwner) so existence doesn't leak across identities. The SSH
// surface therefore never sees ErrNotOwner; mapping it would be dead
// code. A foreign-identity verb attempt is indistinguishable from a
// well-formed-but-missing slug: both exit 4.
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
