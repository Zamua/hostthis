package ssh

import (
	"testing"

	gossh "github.com/charmbracelet/ssh"
)

// fakeColorCtx implements the colorContext interface for table tests.
// hasPty toggles the third return of Pty(); environ is returned as-is
// by Environ(). The Pty struct + window channel content are unused by
// shouldColor, so the zero values are fine.
type fakeColorCtx struct {
	hasPty  bool
	environ []string
}

func (f fakeColorCtx) Pty() (gossh.Pty, <-chan gossh.Window, bool) {
	return gossh.Pty{}, nil, f.hasPty
}

func (f fakeColorCtx) Environ() []string { return f.environ }

// TestShouldColor pins the CLI-convention rule for NO_COLOR + TERM=dumb
// across every combination that matters. Documented at
// https://no-color.org; the helper is the single source of truth the
// SSH surface must consult before writing any ANSI escape.
func TestShouldColor(t *testing.T) {
	cases := []struct {
		name string
		ctx  fakeColorCtx
		want bool
	}{
		{
			name: "PTY + TERM=xterm + no NO_COLOR → color on",
			ctx:  fakeColorCtx{hasPty: true, environ: []string{"TERM=xterm-256color"}},
			want: true,
		},
		{
			name: "PTY + no env at all → color on (no opt-out signal)",
			ctx:  fakeColorCtx{hasPty: true, environ: nil},
			want: true,
		},
		{
			name: "PTY + NO_COLOR=1 → color off",
			ctx:  fakeColorCtx{hasPty: true, environ: []string{"NO_COLOR=1"}},
			want: false,
		},
		{
			name: "PTY + NO_COLOR= (empty value) → color off (presence alone disables per no-color.org)",
			ctx:  fakeColorCtx{hasPty: true, environ: []string{"NO_COLOR="}},
			want: false,
		},
		{
			name: "PTY + NO_COLOR=true alongside TERM=xterm → NO_COLOR wins, color off",
			ctx:  fakeColorCtx{hasPty: true, environ: []string{"TERM=xterm-256color", "NO_COLOR=true"}},
			want: false,
		},
		{
			name: "PTY + TERM=dumb → color off",
			ctx:  fakeColorCtx{hasPty: true, environ: []string{"TERM=dumb"}},
			want: false,
		},
		{
			name: "PTY + TERM=dumb-but-not-exact (e.g. dumb-emacs) → color on (only exact 'dumb' triggers)",
			ctx:  fakeColorCtx{hasPty: true, environ: []string{"TERM=dumb-emacs"}},
			want: true,
		},
		{
			name: "no PTY + TERM=xterm + no NO_COLOR → color off (pipes never get escapes)",
			ctx:  fakeColorCtx{hasPty: false, environ: []string{"TERM=xterm-256color"}},
			want: false,
		},
		{
			name: "no PTY + NO_COLOR=1 → color off (PTY rule alone is sufficient)",
			ctx:  fakeColorCtx{hasPty: false, environ: []string{"NO_COLOR=1"}},
			want: false,
		},
		{
			name: "no PTY + TERM=dumb → color off",
			ctx:  fakeColorCtx{hasPty: false, environ: []string{"TERM=dumb"}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldColor(tc.ctx); got != tc.want {
				t.Fatalf("shouldColor: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSplitEnv pins the parser for "KEY=VALUE" entries as exposed by
// Session.Environ(). Two interesting cases: the "=" must split on the
// FIRST byte (values can contain "="), and a missing "=" yields the
// whole token as the key with an empty value.
func TestSplitEnv(t *testing.T) {
	cases := []struct {
		in      string
		wantKey string
		wantVal string
	}{
		{"NO_COLOR=1", "NO_COLOR", "1"},
		{"NO_COLOR=", "NO_COLOR", ""},
		{"TERM=xterm-256color", "TERM", "xterm-256color"},
		{"PATH=/usr/bin:/bin", "PATH", "/usr/bin:/bin"},
		{"WEIRD=a=b=c", "WEIRD", "a=b=c"}, // values can contain '='
		{"BAREKEY", "BAREKEY", ""},        // no '=' at all
		{"", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotKey, gotVal := splitEnv(tc.in)
			if gotKey != tc.wantKey || gotVal != tc.wantVal {
				t.Fatalf("splitEnv(%q) = (%q, %q), want (%q, %q)",
					tc.in, gotKey, gotVal, tc.wantKey, tc.wantVal)
			}
		})
	}
}
