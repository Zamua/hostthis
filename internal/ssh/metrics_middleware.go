package ssh

import (
	"strconv"
	"time"

	gossh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
)

// CommandRecorder is the port the SSH server uses to report command outcomes.
// A narrow interface rather than a concrete type keeps the metrics
// implementation out of this package and lets tests assert on recorded calls
// without a Prometheus registry.
type CommandRecorder interface {
	RecordCommand(verb, outcome string, d time.Duration)
}

// exitRecorder wraps a session to capture the exit code a verb reports.
//
// Verbs signal their result by calling sess.Exit; nothing is returned up the
// middleware chain. Embedding the session and intercepting that one call is
// what makes the outcome observable without touching a single verb.
type exitRecorder struct {
	gossh.Session
	code int
	set  bool
}

func (e *exitRecorder) Exit(code int) error {
	e.code, e.set = code, true
	return e.Session.Exit(code)
}

// metricsMiddleware times each session and records its verb and outcome.
//
// Placed OUTERMOST in the chain so it also sees sessions the gate rejects.
// Those never reach the dispatcher, and a rise in refusals is exactly the kind
// of thing worth seeing on a graph.
func (s *Server) metricsMiddleware() wish.Middleware {
	return func(next gossh.Handler) gossh.Handler {
		return func(sess gossh.Session) {
			start := time.Now()
			rec := &exitRecorder{Session: sess}
			next(rec)
			s.metrics().RecordCommand(verbLabel(sess.Command()), outcomeLabel(rec), time.Since(start))
		}
	}
}

// verbLabel maps an argv to a BOUNDED label value.
//
// The first argument is attacker-controlled, so it can never become a label
// directly: that would let anyone grow the series count without limit by
// sending random verbs. Only names in the verb registry pass through; anything
// else collapses to "unknown".
func verbLabel(argv []string) string {
	switch {
	case len(argv) == 0:
		// No command at all is the implicit upload: piping content with no
		// verb is the service's primary path, so it earns its own label rather
		// than counting as unknown.
		return "upload"
	case argv[0] == "--help" || argv[0] == "-h":
		return "help"
	case len(argv[0]) > 2 && argv[0][:2] == "--":
		// A leading flag is an upload with options, dispatched as one.
		return "upload"
	}
	if _, ok := lookupVerbDescriptor(argv[0]); ok {
		return argv[0]
	}
	return "unknown"
}

// outcomeLabel reduces an exit code to a bounded outcome.
//
// A session that never called Exit did not complete normally - the client hung
// up, or a handler returned without reporting - and that is worth telling apart
// from a clean success, since both otherwise look like "not an error".
func outcomeLabel(rec *exitRecorder) string {
	switch {
	case !rec.set:
		return "incomplete"
	case rec.code == ExitOK:
		return "ok"
	case rec.code == ExitSybilRefuse:
		return "refused"
	default:
		return "error_" + strconv.Itoa(rec.code)
	}
}
