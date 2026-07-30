package domain

import "testing"

// MaxSiteBytes must equal UserQuotaBytes: a site may fill an identity's whole
// budget and no more. They used to be one expression, but UserQuotaBytes became
// a var so tests can shrink it, and a const cannot reference a var - so the
// equality is now an invariant two declarations must maintain rather than
// something the compiler guarantees.
//
// This pins it. Without it, raising the quota would silently leave the untar
// guard at the old value, and a site deploy would be refused by a limit nobody
// updated - a failure that looks like a bug in the deploy path rather than a
// stale constant.
func TestMaxSiteBytesMatchesQuota(t *testing.T) {
	if MaxSiteBytes != UserQuotaBytes {
		t.Fatalf("MaxSiteBytes (%d) must equal UserQuotaBytes (%d): a site may fill the whole "+
			"per-identity budget and no more. Update both together.", MaxSiteBytes, UserQuotaBytes)
	}
}
