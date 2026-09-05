package domain

import "testing"

// A non-positive cap means unlimited, so an installation with no configured
// cap does not reject every write.
func TestAllowance_NonPositiveCapIsUnlimited(t *testing.T) {
	for _, cap := range []int64{0, -1} {
		a := Allowance{Cap: cap, Used: 1 << 40}
		if !a.Unlimited() {
			t.Fatalf("cap %d must read as unlimited", cap)
		}
	}
}

func TestAllowance_Remaining(t *testing.T) {
	if got := (Allowance{Cap: 100, Used: 30}).Remaining(); got != 70 {
		t.Fatalf("Remaining: want 70, got %d", got)
	}
	// Floored at zero: the site extractor consumes this as a byte budget.
	if got := (Allowance{Cap: 100, Used: 250}).Remaining(); got != 0 {
		t.Fatalf("an over-quota identity must report 0 remaining, not a negative budget; got %d", got)
	}
}
