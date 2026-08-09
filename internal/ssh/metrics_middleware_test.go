package ssh

import "testing"

// Pins that verb labels come from a bounded set. The first argument is
// attacker-controlled, so anything unrecognised must collapse to a single
// placeholder; without that, sending random verbs grows the Prometheus series
// count without limit.
func TestVerbLabel_IsBounded(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"no command is the implicit upload", nil, "upload"},
		{"leading flag is an upload with options", []string{"--name", "x"}, "upload"},
		{"long help flag", []string{"--help"}, "help"},
		{"short help flag", []string{"-h"}, "help"},
		{"known verb passes through", []string{"list"}, "list"},
		{"another known verb", []string{"delete", "abc"}, "delete"},
		{"unknown verb collapses", []string{"definitely-not-a-verb"}, "unknown"},
		{"attacker-supplied garbage collapses", []string{"../../etc/passwd"}, "unknown"},
		{"empty first arg collapses", []string{""}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verbLabel(tc.argv); got != tc.want {
				t.Fatalf("verbLabel(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// Pins that a session which never reported an exit code is distinguishable
// from a clean success. Both are "not an error", and collapsing them would
// hide clients hanging up mid-command.
func TestOutcomeLabel(t *testing.T) {
	cases := []struct {
		name string
		rec  *exitRecorder
		want string
	}{
		{"never exited", &exitRecorder{}, "incomplete"},
		{"clean exit", &exitRecorder{code: ExitOK, set: true}, "ok"},
		{"gate refusal", &exitRecorder{code: ExitSybilRefuse, set: true}, "refused"},
		{"usage error", &exitRecorder{code: ExitUsage, set: true}, "error_2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outcomeLabel(tc.rec); got != tc.want {
				t.Fatalf("outcomeLabel = %q, want %q", got, tc.want)
			}
		})
	}
}
