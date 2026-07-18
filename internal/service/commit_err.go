package service

import (
	"errors"

	"github.com/Zamua/hostthis/internal/storage"
)

// commitErrClass is the classification classifyCommitErr assigns a
// commit-path storage error.
type commitErrClass int

const (
	// commitOK: err was nil.
	commitOK commitErrClass = iota
	// commitServiceFull: the durable total-bytes ceiling (the object
	// store's bucket quota) rejected the write; translated ErrServiceFull.
	commitServiceFull
	// commitOverQuota: the per-identity cap rejected the write;
	// translated ErrOverQuota.
	commitOverQuota
	// commitSlugTaken: the chosen slug/id collided; translated
	// SlugTakenErr. Retry loops re-mint on this class instead of
	// returning the translated error.
	commitSlugTaken
	// commitOther: unclassified; the error passes through VERBATIM so
	// call sites keep their own default handling (wrap with context,
	// check further sentinels, surface as-is).
	commitOther
)

// classifyCommitErr is the ONE translation of the storage commit-error
// triad (service-full / over-user-quota / slug-taken) into the service
// vocabulary. Every write path - upload create (both blob modes), paste
// update, site deploy/redeploy - repeats this exact mapping, so it
// lives here once; the call sites keep only their path-specific cases
// (re-mint on collision, wrap a stage failure, translate not-found).
//
// Returns the class plus the translated error: nil for commitOK, the
// service sentinel for the triad classes, and err itself (the same
// value, not a copy) for commitOther. Sentinels are matched with
// errors.Is, so wrapping anywhere in the storage stack is fine.
func classifyCommitErr(err error) (commitErrClass, error) {
	switch {
	case err == nil:
		return commitOK, nil
	case errors.Is(err, storage.ErrServiceFull):
		return commitServiceFull, ErrServiceFull
	case errors.Is(err, storage.ErrOverUserQuota):
		return commitOverQuota, ErrOverQuota
	case isSlugTaken(err):
		return commitSlugTaken, SlugTakenErr
	default:
		return commitOther, err
	}
}
