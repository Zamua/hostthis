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
func (a Allowance) Remaining() int64 {
	if a.Unlimited() {
		return 0
	}
	return max(a.Cap-a.Used, 0)
}
