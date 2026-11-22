package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultPruneKeep is how many of the newest live versions `prune` keeps when
// the owner does not say.
const DefaultPruneKeep = 3

// ServedVersion is the version a paste's URL serves: its pin, or else the
// newest live version. vers is newest first, tombstones included. Zero means
// no live version.
func ServedVersion(vers []Version, pinned int) int {
	if pinned > 0 {
		return pinned
	}
	for _, v := range vers {
		if !v.Deleted {
			return v.VerNum
		}
	}
	return 0
}

// ParseVersionRange parses "3-120" (either end may carry a "v") into an
// inclusive range. ok is false when s is not a range at all, so the caller can
// fall back to a single version.
func ParseVersionRange(s string) (lo, hi int, ok bool, err error) {
	a, b, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, false, nil
	}
	lo, errLo := strconv.Atoi(strings.TrimPrefix(a, "v"))
	hi, errHi := strconv.Atoi(strings.TrimPrefix(b, "v"))
	if errLo != nil || errHi != nil || lo < 1 || hi < lo {
		return 0, 0, true, fmt.Errorf("invalid version range %q (want <from>-<to>, like 3-120)", s)
	}
	return lo, hi, true, nil
}

// VersionsInRange picks the live versions numbered lo..hi, leaving out the
// served one. vers is newest first; the result keeps that order.
func VersionsInRange(vers []Version, served, lo, hi int) []Version {
	var out []Version
	for _, v := range vers {
		if !v.Deleted && v.VerNum != served && v.VerNum >= lo && v.VerNum <= hi {
			out = append(out, v)
		}
	}
	return out
}

// VersionsToPrune picks every live version except the newest keep live ones
// and the served one. vers is newest first; the result keeps that order.
func VersionsToPrune(vers []Version, served, keep int) []Version {
	var out []Version
	kept := 0
	for _, v := range vers {
		switch {
		case v.Deleted:
		case v.VerNum == served:
		case kept < keep:
			kept++
		default:
			out = append(out, v)
		}
	}
	return out
}

// QuotaWarnPercent is the share of the quota past which a successful upload
// warns its owner, before uploads start being refused.
const QuotaWarnPercent = 80

// QuotaNearlyFull reports whether used is past QuotaWarnPercent of quota.
func QuotaNearlyFull(used, quota int) bool {
	return quota > 0 && used*100 > quota*QuotaWarnPercent
}
