package ssh

import "testing"

// TestExitCodeConstants_PinValues pins the integer values of every
// exported exit-code constant. The wire-level meaning of these codes
// is documented in docs/SPEC.md > "Exit codes" and ships in
// release-note guidance for script authors; rebucketing a value
// silently is a breaking change for any operator who's keyed a CI
// pipeline or shell pipeline off `$?`. The characterization tests
// (`TestExitCodes_Characterization`) pin the per-path mapping; this
// test pins the underlying values so a refactor that touches the
// constant block can't drift either layer.
//
// 5 is intentionally absent: the slot is reserved (see the const
// block in server.go for context) and has no exported constant to
// pin. Adding a new code? Pick the next free slot (7 onward) and
// extend both this test and the SPEC table; do NOT reuse 5.
func TestExitCodeConstants_PinValues(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"ExitOK", ExitOK, 0},
		{"ExitErr", ExitErr, 1},
		{"ExitUsage", ExitUsage, 2},
		{"ExitAuth", ExitAuth, 3},
		{"ExitNotFound", ExitNotFound, 4},
		{"ExitSybilRefuse", ExitSybilRefuse, 6},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (changing this value is a breaking change for any script that branches on the SSH exit code)", tc.name, tc.got, tc.want)
		}
	}
}
