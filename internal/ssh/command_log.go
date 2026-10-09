package ssh

import (
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	gossh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
)

// commandLogMiddleware logs one line per session: verb, slug, outcome and
// duration, so load on the cells can be traced to the commands behind it.
// Only bounded or public values are logged: the verb from the registry and a
// slug that parses as one. Names, labels and payloads never are.
func (s *Server) commandLogMiddleware() wish.Middleware {
	return func(next gossh.Handler) gossh.Handler {
		return func(sess gossh.Session) {
			start := time.Now()
			rec := &exitRecorder{Session: sess}
			next(rec)
			verb, slug := commandFields(sess.Command())
			s.Logger.Printf("ssh: command verb=%s slug=%s outcome=%s ms=%d",
				verb, slug, outcomeLabel(rec), time.Since(start).Milliseconds())
		}
	}
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
