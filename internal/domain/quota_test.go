package domain

import (
	"errors"
	"testing"
)

// The boundary: landing exactly on the cap is allowed, one byte past is not.
// `>=` reads as naturally as `>` to anyone who has not checked.
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
	// rewrite naturally reaches for an early `used >= cap` reject, which changes
	// this silently.
	full := Allowance{Cap: 100, Used: 100}
	if err := full.Admit(0); err != nil {
		t.Fatalf("a zero-byte write at the cap must be admitted (the old arithmetic did), got %v", err)
	}
	if err := full.Admit(1); !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("one byte at the cap must be rejected, got %v", err)
	}
}

// A non-positive cap means unlimited. Installations with no configured cap
// must not start rejecting every write.
func TestAllowance_NonPositiveCapIsUnlimited(t *testing.T) {
	for _, cap := range []int64{0, -1} {
		a := Allowance{Cap: cap, Used: 1 << 40}
		if !a.Unlimited() {
			t.Fatalf("cap %d must read as unlimited", cap)
		}
		if err := a.Admit(1 << 40); err != nil {
			t.Fatalf("cap %d must admit any write, got %v", cap, err)
		}
		if err := a.AdmitReplacing(0, 1<<40); err != nil {
			t.Fatalf("cap %d must admit any replacement, got %v", cap, err)
		}
	}
}

// Replacing a record credits the bytes it displaces. Without it, redeploying at
// the same size is charged twice and an identity at its limit cannot update in
// place.
func TestAllowance_AdmitReplacingCreditsTheOldBytes(t *testing.T) {
	// Exactly at the cap, replacing like for like.
	a := Allowance{Cap: 100, Used: 100}
	if err := a.AdmitReplacing(40, 40); err != nil {
		t.Fatalf("replacing a 40-byte record with another 40-byte record must be admitted even at "+
			"the cap - the old bytes are about to stop existing; got %v", err)
	}
	// Growing by more than the headroom the credit provides.
	if err := a.AdmitReplacing(40, 41); !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("growing past the cap must still be rejected, got %v", err)
	}
	// Shrinking always fits.
	if err := a.AdmitReplacing(40, 1); err != nil {
		t.Fatalf("shrinking a record must be admitted, got %v", err)
	}
}

// Admit and AdmitReplacing must differ: a replacement ignoring the credit would
// be indistinguishable from a plain add.
func TestAllowance_ReplacingIsNotThePlainAdd(t *testing.T) {
	a := Allowance{Cap: 100, Used: 100}
	if err := a.Admit(40); !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("setup: a plain add of 40 at the cap must be rejected, got %v", err)
	}
	if err := a.AdmitReplacing(40, 40); err != nil {
		t.Fatalf("the same 40 bytes as a REPLACEMENT must be admitted; if this matches Admit's "+
			"answer then the credit is not being applied: %v", err)
	}
}

func TestAllowance_Remaining(t *testing.T) {
	if got := (Allowance{Cap: 100, Used: 30}).Remaining(); got != 70 {
		t.Fatalf("Remaining: want 70, got %d", got)
	}
	// Floored at zero: the site extractor takes this as a byte budget.
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
