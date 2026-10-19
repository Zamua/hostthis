package service

import (
	"context"
	"errors"
	"log"

	"github.com/Zamua/hostthis/internal/domain"
)

// discardUpload deletes an upload's objects. A failure is logged, not
// returned: the caller's outcome is already decided, and a leaked prefix costs
// space, never a read. An empty id is a version recorded without an upload id,
// which owns nothing to delete.
func discardUpload(blob BlobUnit, logger *log.Logger, uploadID string) {
	if uploadID == "" {
		return
	}
	if err := blob.DeleteUpload(context.Background(), uploadID); err != nil && logger != nil {
		logger.Printf("upload %s: delete objects: %v", uploadID, err)
	}
}

// discardRelease deletes what a version delete freed. Failures are logged, as
// for discardUpload.
func discardRelease(blob BlobUnit, logger *log.Logger, rel domain.VersionRelease) {
	if !rel.Listed {
		discardUpload(blob, logger, rel.Upload)
		return
	}
	for _, prefix := range rel.Prefixes {
		id, ok := domain.UploadIDOfPrefix(prefix)
		if !ok {
			if logger != nil {
				logger.Printf("release: skip malformed prefix %q", prefix)
			}
			continue
		}
		discardUpload(blob, logger, id)
	}
	for _, key := range rel.Keys {
		if err := blob.DeleteObject(context.Background(), key); err != nil && logger != nil {
			logger.Printf("object %s: delete: %v", key, err)
		}
	}
}

// abandonUpload discards the objects staged for a metadata write that failed,
// but only when the failure proves the write never lands. A lost response can
// still publish later through the backend's recovery, and deleting its bytes
// would break the version it publishes, so those objects are kept.
func abandonUpload(blob BlobUnit, logger *log.Logger, uploadID string, commitErr error) {
	if refusedCommit(commitErr) {
		discardUpload(blob, logger, uploadID)
		return
	}
	if logger != nil {
		logger.Printf("upload %s: metadata outcome unknown, objects kept: %v", uploadID, commitErr)
	}
}

// refusedCommit reports whether a metadata write was definitively refused.
func refusedCommit(err error) bool {
	for _, refusal := range []error{
		domain.ErrOverUserQuota, domain.ErrSlugTaken,
		domain.ErrNotFound, domain.ErrTooManyFiles, domain.ErrBusy,
		domain.ErrBaseMoved, domain.ErrBadReuse,
	} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}
