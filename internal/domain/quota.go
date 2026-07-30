package domain

// Allowance is an identity's quota position: how much it may hold, and how much
// it already does. Constructed per check, since Used is derived by scanning the
// enumeration indexes rather than stored.
type Allowance struct {
	// Cap is the ceiling in bytes. Zero or negative means unlimited.
	Cap int64
	// Used is the bytes the identity occupies across every record kind that
	// counts against the cap.
	Used int64
}

func (a Allowance) Unlimited() bool { return a.Cap <= 0 }

// Remaining is how many more bytes the identity may take, floored at zero.
// Meaningless when Unlimited: a caller needing that case must check first.
//
// The site upload path uses it as an extraction budget, before the archive's
// expanded size is known.
func (a Allowance) Remaining() int64 {
	if a.Unlimited() {
		return 0
	}
	return max(a.Cap-a.Used, 0)
}

// Admit reports whether accepting incoming more bytes stays within the cap.
// Landing exactly on the cap is allowed.
func (a Allowance) Admit(incoming int64) error {
	if a.Unlimited() {
		return nil
	}
	if incoming <= 0 {
		// Cannot push anyone over, and must stay admissible for an identity
		// already at the cap.
		return nil
	}
	// Compared against headroom rather than by summing: a large enough incoming
	// makes used+incoming overflow and wrap into acceptance.
	if a.Used >= a.Cap {
		return ErrOverUserQuota
	}
	if incoming > a.Cap-a.Used {
		return ErrOverUserQuota
	}
	return nil
}

// AdmitReplacing reports whether swapping a record of oldBytes for one of
// newBytes stays within the cap. The displaced bytes are credited back: they
// are still counted in Used but are about to stop existing, and without the
// credit a same-size redeploy is charged twice, so an identity at its limit
// could never update in place.
//
// Separate from Admit rather than Admit(new-old) so the call site states intent
// and cannot invert the sign.
func (a Allowance) AdmitReplacing(oldBytes, newBytes int64) error {
	if a.Unlimited() {
		return nil
	}
	// Floored at zero: a credit exceeding the total would manufacture headroom.
	credited := max(a.Used-oldBytes, 0)
	return Allowance{Cap: a.Cap, Used: credited}.Admit(newBytes)
}
