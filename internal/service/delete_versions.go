package service

import (
	"errors"

	"github.com/Zamua/hostthis/internal/domain"
)

// VersionPicker chooses which of a slug's versions to delete, given its
// newest-first timeline and the version its URL serves (domain.VersionsInRange,
// domain.VersionsToPrune).
type VersionPicker func(vers []domain.Version, served int) []domain.Version

// DeleteVersionsResult reports a multi-version delete. Picked is what the
// picker chose; Deleted is what was actually freed (empty on a dry run).
type DeleteVersionsResult struct {
	Picked     []domain.Version
	Deleted    []DeleteVersionResult
	FreedBytes int
}

// DeleteVersions deletes every version pick chooses, one at a time, through
// DeleteVersion, so each keeps its own guards (owner, tombstone, served). A
// version that turns out to be served or already gone is skipped, not an
// error. A dry run only reports the pick. On any other error it stops and
// returns what it freed so far with the error.
func (m *Manage) DeleteVersions(slug domain.Slug, owner domain.Owner, pick VersionPicker, dryRun bool) (DeleteVersionsResult, error) {
	paste, err := m.requireOwner(slug, owner)
	if err != nil {
		return DeleteVersionsResult{}, err
	}
	vers, err := m.Repo.ListVersions(slug)
	if err != nil {
		return DeleteVersionsResult{}, err
	}
	res := DeleteVersionsResult{Picked: pick(vers, domain.ServedVersion(vers, paste.PinnedVersion))}
	if dryRun {
		return res, nil
	}
	for _, v := range res.Picked {
		dr, err := m.DeleteVersion(slug, owner, v.VerNum)
		switch {
		case err == nil:
			res.Deleted = append(res.Deleted, dr)
			res.FreedBytes += dr.FreedBytes
		case errors.Is(err, ErrVersionAlreadyDeleted), errors.Is(err, ErrVersionCurrentlyServed):
		default:
			return res, err
		}
	}
	return res, nil
}
