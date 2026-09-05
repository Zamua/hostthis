package service

import (
	"errors"

	"github.com/Zamua/hostthis/internal/domain"
)

// commitErrClass is the classification classifyCommitErr assigns a commit-path
// storage error.
type commitErrClass int

const (
	commitOK commitErrClass = iota
	// commitServiceFull: the object store's bucket quota rejected the write.
	commitServiceFull
	// commitOverQuota: the per-identity cap rejected the write.
	commitOverQuota
	// commitSlugTaken: the chosen slug collided. A retry loop re-mints on this
	// class instead of returning the translated error.
	commitSlugTaken
	// commitOther: unclassified, so the error passes through VERBATIM and call
	// sites keep their own default handling.
	commitOther
)

// classifyCommitErr is the ONE translation of the storage commit-error triad
// (service-full, over-user-quota, slug-taken) into the service vocabulary, so
// call sites keep only their path-specific cases. The returned error is nil
// for commitOK, the service sentinel for the triad, and err itself for
// commitOther. Sentinels match with errors.Is, so wrapping is fine.
func classifyCommitErr(err error) (commitErrClass, error) {
	switch {
	case err == nil:
		return commitOK, nil
	case errors.Is(err, domain.ErrServiceFull):
		return commitServiceFull, ErrServiceFull
	case errors.Is(err, domain.ErrOverUserQuota):
		return commitOverQuota, ErrOverQuota
	case errors.Is(err, domain.ErrSlugTaken):
		return commitSlugTaken, ErrSlugTaken
	default:
		return commitOther, err
	}
}
