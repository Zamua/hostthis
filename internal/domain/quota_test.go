package domain

import (
	"errors"
	"testing"
)

// The boundary: landing exactly on the cap is allowed, one byte past is not.
func TestAllowance_AdmitBoundary(t *testing.T) {
	a := Allowance{Cap: 100, Used: 90}

	if err := a.Admit(10); err != nil {
		t.Fatalf("landing exactly on the cap must be admitted, got %v", err)
	}
	if err := a.Admit(11); !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("one byte past the cap must be rejected with ErrOverUserQuota, got %v", err)
	}
	if err := a.Admit(0); err != nil {
		t.Fatalf("a zero-byte write must be admitted, got %v", err)
	}

	// At the cap, a zero-byte write must still be admitted. An overflow-safe
	// rewrite naturally reaches for an early `used >= cap` reject, which
	// changes this silently.
	full := Allowance{Cap: 100, Used: 100}
	if err := full.Admit(0); err != nil {
		t.Fatalf("a zero-byte write at the cap must be admitted (the old arithmetic did), got %v", err)
	}
	if err := full.Admit(1); !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("one byte at the cap must be rejected, got %v", err)
	}
}

// A non-positive cap means unlimited, so an installation with no configured
// cap does not reject every write.
func TestAllowance_NonPositiveCapIsUnlimited(t *testing.T) {
	for _, cap := range []int64{0, -1} {
		a := Allowance{Cap: cap, Used: 1 << 40}
		if !a.Unlimited() {
			t.Fatalf("cap %d must read as unlimited", cap)
		}
		if err := a.Admit(1 << 40); err != nil {
			t.Fatalf("cap %d must admit any write, got %v", cap, err)
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

// A large incoming size must not wrap negative and silently admit.
func TestAllowance_DoesNotWrapOnHugeValues(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	a := Allowance{Cap: 100, Used: 50}
	if err := a.Admit(maxInt64); !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("a huge incoming size must be rejected, not wrapped into acceptance; got %v", err)
	}
}
