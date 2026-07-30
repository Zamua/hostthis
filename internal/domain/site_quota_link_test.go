package domain

import "testing"

// MaxSiteBytes must equal UserQuotaBytes: a site may fill an identity's whole
// budget and no more.
//
// A const cannot reference a var, and UserQuotaBytes is a var so tests can
// shrink it, so the equality is an invariant two declarations must maintain
// rather than one the compiler enforces.
func TestMaxSiteBytesMatchesQuota(t *testing.T) {
	if MaxSiteBytes != UserQuotaBytes {
		t.Fatalf("MaxSiteBytes (%d) must equal UserQuotaBytes (%d): a site may fill the whole "+
			"per-identity budget and no more. Update both together.", MaxSiteBytes, UserQuotaBytes)
	}
}
