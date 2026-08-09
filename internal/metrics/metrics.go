// Package metrics owns this process's Prometheus collectors.
//
// It exists because the SSH surface is otherwise unobservable. A reverse proxy
// routes SSH as plain TCP, so it can report how many connections are open and
// nothing else: no command counts, no durations, no failures. Everything about
// what users actually do here has to come from inside the process.
//
// Deliberately narrow. Each collector below answers a question worth asking of
// a running deployment; anything that only answers "what is the code doing" is
// a log line, not a metric.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the set of collectors, constructed against an explicit registry so
// tests get a clean one and never observe another test's counts.
type Metrics struct {
	commands *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// New registers the collectors and returns them. Passing a fresh
// prometheus.NewRegistry() in tests keeps them isolated.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		commands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hostthis_ssh_commands_total",
			Help: "SSH commands handled, by verb and outcome.",
		}, []string{"verb", "outcome"}),

		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "hostthis_ssh_command_duration_seconds",
			Help: "Wall time to handle an SSH command, by verb.",
			// Spread deliberately wide. A read should land in single-digit
			// milliseconds while an upload legitimately takes seconds, and the
			// default buckets top out too low to distinguish "slow" from
			// "pathological" on the write path.
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"verb"}),
	}
	reg.MustRegister(m.commands, m.duration)
	return m
}

// RecordCommand records one handled command.
//
// CARDINALITY: verb and outcome must both come from bounded sets. They are
// derived from user input, and a caller that passed the raw first argument
// through would let anyone mint unbounded label values by sending random verbs,
// which grows the series count without limit. Callers map unknown input to a
// fixed placeholder.
func (m *Metrics) RecordCommand(verb, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.commands.WithLabelValues(verb, outcome).Inc()
	m.duration.WithLabelValues(verb).Observe(d.Seconds())
}
