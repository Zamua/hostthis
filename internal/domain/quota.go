package domain

// The per-identity byte quota, as a rule rather than as arithmetic scattered
// through the adapters.
//
// This existed in thirteen places, one per write path across four repositories,
// and they did not agree. Some were a plain `used + body > cap`. Some credited
// the bytes of a record being REPLACED (`used - creditOld + body > cap`). Two
// expressed that same credit in different algebra (`used - old + new` versus
// `used + (new - old)`). Some summed paste and site totals, some did not. That
// is not thirteen call sites of one rule, it is thirteen opportunities for the
// rule to drift - and the subtle case, replace-credits-the-old-size, is exactly
// the kind that gets corrected in one place and missed in the other twelve.
//
// What stays in the adapters is the QUERY: computing how many bytes an identity
// currently occupies means scanning an enumeration index, which is squarely
// infrastructure. What moves here is the DECISION.

// Allowance is an identity's quota position at a moment: how much they may
// hold, and how much they already do.
//
// A value object - constructed per check, never stored. `Used` is derived by
// scanning the enumeration indexes rather than kept as a counter, so it is
// always a fresh observation and never a durable number that can drift.
type Allowance struct {
	// Cap is the ceiling in bytes. Zero or negative means UNLIMITED, which is
	// how the repositories have always spelled "no cap configured"; preserved
	// here so the meaning lives in one place instead of being re-derived at
	// each call site.
	Cap int64
	// Used is the bytes the identity currently occupies, across every record
	// kind that counts against the cap.
	Used int64
}

// Unlimited reports whether no ceiling applies.
func (a Allowance) Unlimited() bool { return a.Cap <= 0 }

// Remaining is how many more bytes the identity may take, floored at zero.
// Returns a large sentinel-free value only when a cap applies; callers that
// need "unlimited" should check Unlimited first.
//
// Used by the site upload path, which needs a budget to hand the extractor
// BEFORE it knows the archive's true expanded size.
func (a Allowance) Remaining() int64 {
	if a.Unlimited() {
		return 0
	}
	return max(a.Cap-a.Used, 0)
}

// Admit reports whether accepting incoming more bytes keeps the identity within
// its cap, returning ErrOverUserQuota if not.
//
// The comparison is STRICTLY GREATER: landing exactly on the cap is allowed.
// That boundary was consistent across the thirteen copies and is preserved
// deliberately - a user whose total lands exactly on the limit is at the limit,
// not over it.
func (a Allowance) Admit(incoming int64) error {
	if a.Unlimited() {
		return nil
	}
	// Compared as "incoming exceeds the headroom" rather than "used plus
	// incoming exceeds the cap", because the latter OVERFLOWS: a large enough
	// incoming wraps the sum negative, which compares as under the cap and
	// silently admits. Every one of the thirteen adapter copies had that shape.
	// Not reachable with real sizes - it needs an exabyte-scale value - but the
	// safe form costs nothing and removes the question.
	if incoming <= 0 {
		// Cannot push anyone over, and must stay admissible even for an
		// identity already AT the cap: the old arithmetic (used + 0 > cap)
		// admitted it, and rejecting a zero-byte write would be a silent
		// behaviour change dressed as a refactor.
		return nil
	}
	if a.Used >= a.Cap {
		return ErrOverUserQuota
	}
	if incoming > a.Cap-a.Used {
		return ErrOverUserQuota
	}
	return nil
}

// AdmitReplacing reports whether swapping a record of oldBytes for one of
// newBytes keeps the identity within its cap.
//
// The replaced record's bytes are CREDITED BACK, because at the moment of the
// check they are still counted in Used but are about to stop existing. Without
// the credit, redeploying a site at the same size would be charged twice and a
// user near their limit could never update in place - they would have to delete
// and re-upload, which is strictly worse for them and for us.
//
// Separate from Admit rather than expressed as Admit(new - old) because the
// intent is what matters at the call site: a reader can see that this path
// replaces rather than adds, and cannot accidentally get the sign wrong.
func (a Allowance) AdmitReplacing(oldBytes, newBytes int64) error {
	if a.Unlimited() {
		return nil
	}
	// Credit first, then defer to Admit so the overflow-safe comparison lives
	// in exactly one place. Floored at zero: a credit larger than the current
	// total would otherwise make Used negative and manufacture headroom.
	credited := max(a.Used-oldBytes, 0)
	return Allowance{Cap: a.Cap, Used: credited}.Admit(newBytes)
}
