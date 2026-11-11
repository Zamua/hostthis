package ssh

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	gossh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
)

// commandLogMiddleware logs one line per session: verb, slug, outcome and
// duration, so load on the cells can be traced to the commands behind it, and
// who ran it from where: a short hash of the key, how the session logged in,
// and the client's address and subnet, so a question about a key's use can be
// answered from the log. The verb comes from the registry and the slug must
// parse as one; names, labels and payloads are never logged.
func (s *Server) commandLogMiddleware() wish.Middleware {
	return func(next gossh.Handler) gossh.Handler {
		return func(sess gossh.Session) {
			start := time.Now()
			rec := &exitRecorder{Session: sess}
			next(rec)
			verb, slug := commandFields(sess.Command())
			ip := remoteIP(sess)
			s.Logger.Printf("ssh: command verb=%s slug=%s outcome=%s ms=%d key=%s auth=%s addr=%s subnet=%s",
				verb, slug, outcomeLabel(rec), time.Since(start).Milliseconds(),
				sessionKeyHash(sess), sessionAuthMethod(sess), domain.AddrText(ip), domain.SubnetOf(ip))
			if rec.set && s.Abuse != nil {
				if event, ok := abuseEvent(rec.code); ok {
					s.Abuse.Log(event, "ssh", rec.code, ip)
				}
			}
		}
	}
}

// sessionKeyHash is the short hash operator tools and the audit log name an
// owner by (the first 8 bytes of the SHA-256 of the identity, in hex), or "-"
// for a session that authenticated without a key.
func sessionKeyHash(sess gossh.Session) string {
	owner, err := domain.OwnerFromKeyFingerprint(sessionKeyFingerprint(sess))
	if err != nil {
		return "-"
	}
	sum := sha256.Sum256([]byte(owner.String()))
	return hex.EncodeToString(sum[:8])
}

// authMethodKey is the connection-context key under which the password and
// keyboard-interactive callbacks note themselves. It is for the log only:
// identity comes from the verified key (sessionKeyFingerprint).
const authMethodKey = "hostthisAuthMethod"

// sessionAuthMethod names how the session authenticated.
func sessionAuthMethod(sess gossh.Session) string {
	if sess.PublicKey() != nil {
		return "publickey"
	}
	if method, ok := sess.Context().Value(authMethodKey).(string); ok {
		return method
	}
	return "none"
}

// abuseEvent classifies the exit codes that are refusals of the client:
// every other code is a normal outcome and logs nothing.
func abuseEvent(code int) (domain.AbuseEvent, bool) {
	switch code {
	case ExitSybilRefuse, ExitKeyBusy:
		return domain.AbuseRateLimited, true
	case ExitAuth, ExitBanned:
		return domain.AbuseAuthFailure, true
	case ExitRefused:
		return domain.AbuseRejected, true
	}
	return "", false
}

// commandFields names the verb and the slug a command targets, "-" when it
// targets none. A first argument outside the registry that parses as a slug is
// the update shortcut, as the dispatcher reads it.
func commandFields(argv []string) (verb, slug string) {
	verb, slug = verbLabel(argv), "-"
	switch verb {
	case "unknown":
		if s, err := domain.ParseSlug(argv[0]); err == nil {
			return "update", s.String()
		}
	case "upload", "help":
	default:
		if len(argv) > 1 && !strings.HasPrefix(argv[1], "-") {
			if s, err := domain.ParseSlug(argv[1]); err == nil {
				slug = s.String()
			}
		}
	}
	return verb, slug
}
