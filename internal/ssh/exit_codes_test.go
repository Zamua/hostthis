package ssh

import "testing"

// TestExitCodeConstants_PinValues pins the integer value of every exported
// exit-code constant. These are a public contract (docs/SPEC.md "Exit codes"),
// so rebucketing one breaks any pipeline branching on `$?`.
// TestExitCodes_Characterization pins the per-path mapping; this pins the
// underlying values so a refactor of the constant block cannot drift either
// layer.
//
// 5 is intentionally absent: the slot is reserved and has no exported
// constant. A new code takes the next free slot (7 onward) and extends both
// this test and the SPEC table; do NOT reuse 5.
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
