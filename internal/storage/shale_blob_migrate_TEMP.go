//go:build slatedb

// ============================================================================
// TEMPORARY phase-4 migration code - DELETE THIS FILE after the prod blob
// migration completes.
//
// This file exists ONLY to support the one-time legacy-blob re-key
// (docs/design/shale-blobs-phase4-migration.md). It is driven exclusively by
// the temporary cmd/hostthis-blob-migrate binary, which runs the migration AS
// the prod hostthis cluster in migrate mode (storage.NewShaleRepo with the
// app's own env + node identity). Nothing on the serving path
// (upload/manage/http/ssh) calls RebindLegacyBlob.
//
// REMOVAL (after the migration is verified GREEN and the rollback grace has
// elapsed): rm this file, rm -r cmd/hostthis-blob-migrate, revert the one temp
// Dockerfile line. No other file references RebindLegacyBlob or LegacyBlobKind.
// ============================================================================

package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
)

// LegacyBlobKind names which legacy record-blob a RebindLegacyBlob call
// re-keys: the paste head, a specific version, or a site file. The migrate
// loop enumerates record-blobs and tags each with its kind.
type LegacyBlobKind int

const (
	// LegacyBlobPaste re-keys a paste head row (pastes/<slug>) and its serving
	// version row (the version whose ContentSHA matches), mirroring how
	// insertAuthoritative stamps the blob id on BOTH on a fresh upload.
	LegacyBlobPaste LegacyBlobKind = iota
	// LegacyBlobVersion re-keys a single version row (versions/<slug>/<NNNN>),
	// identified by its ContentSHA.
	LegacyBlobVersion
	// LegacyBlobSiteFile re-keys a site file: sets sites/<slug>'s
	// FileBlobs[contentSHA] side-table entry.
	LegacyBlobSiteFile
)

// RebindLegacyBlob re-keys ONE legacy record-blob whose bytes have ALREADY been
// staged into the collocated blob bucket (the caller stages first via
// StageBlobStream and passes the resulting ref). In one routed {slug}
// transaction - pinned on RouteKeyForSlug(slug), the same {slug} shard the bref's
// {slug} hash tag co-routes to - it (a) reads the target row, (b) sets the row's
// BlobID (or the site row's FileBlobs[contentSHA]) to ref.BlobID, and (c) binds
// the pointer. It REUSES the existing authoritative-write + BindBlob plumbing
// (runAuthoritative + the bind callback that fires tx.BindBlob), so the row
// update and the bref write co-commit, R=2-replicated by the cluster, IDENTICALLY
// to how a fresh paste/site binds its blob in phase 3. No bind logic is copied.
//
// ref MUST come from StageBlobStream(RouteKeyForSlug(slug), oldBytes, size,
// contentSHA) so it carries the routed Unit + RouteShard + the minted BlobID +
// the content sha; a zero-value ref would produce a malformed bref key.
//
// Idempotent: if the target row already carries ref.BlobID for this sha, the row
// Put is skipped; the pointer bind still fires (an idempotent tx.Put of the bref
// key) so a re-run after a row-set-but-bind-lost crash still lands the pointer.
// A cold re-run is therefore correct (the caller's pre-stage skip avoids minting
// a fresh blobid for an already-rebound row).
func (r *ShaleRepo) RebindLegacyBlob(ctx context.Context, slug domain.Slug, kind LegacyBlobKind, contentSHA string, ref cluster.BlobRef) error {
	if r.kv == nil {
		return errors.New("shale: RebindLegacyBlob requires a blob-configured cluster (cfg.BlobStore was nil)")
	}
	if ref.BlobID == "" {
		return errors.New("shale: RebindLegacyBlob given a zero-value ref (stage the bytes first)")
	}

	pinKey := r.RouteKeyForSlug(slug.String())
	refs := []cluster.BlobRef{ref}

	switch kind {
	case LegacyBlobPaste:
		return r.rebindPasteHead(slug, contentSHA, ref, pinKey, refs)
	case LegacyBlobVersion:
		verNum, err := r.legacyVersionNumForSHA(slug, contentSHA)
		if err != nil {
			return err
		}
		return r.rebindVersion(slug, verNum, ref, pinKey, refs)
	case LegacyBlobSiteFile:
		return r.rebindSiteFile(slug, contentSHA, ref, pinKey, refs)
	default:
		return fmt.Errorf("shale: RebindLegacyBlob unknown kind %d", kind)
	}
}

// legacyVersionNumForSHA finds the version row whose ContentSHA matches sha and
// returns its VerNum, so the rebind can target versions/<slug>/<NNNN> directly.
// The scan is OUTSIDE the tx (a routed read); the rebind re-reads the exact
// version key INSIDE the tx for CAS safety.
func (r *ShaleRepo) legacyVersionNumForSHA(slug domain.Slug, sha string) (int, error) {
	versions, err := r.scanVersions(slug)
	if err != nil {
		return 0, err
	}
	for _, v := range versions {
		if v.ContentSHA == sha {
			return v.VerNum, nil
		}
	}
	return 0, fmt.Errorf("shale: no version row for slug %q sha %q: %w", slug, sha, ErrNotFound)
}

// rebindPasteHead sets BlobID on pastes/<slug> AND on the serving version row
// (the version whose ContentSHA == sha), mirroring insertAuthoritative stamping
// the blob id on both the head and its version. Both Puts + the bind co-commit
// in the one {slug} CAS.
func (r *ShaleRepo) rebindPasteHead(slug domain.Slug, sha string, ref cluster.BlobRef, pinKey []byte, refs []cluster.BlobRef) error {
	pasteKey := shaleKeyPaste(slug)
	// Resolve the serving version's number OUTSIDE the tx; the tx re-reads the
	// exact key for CAS safety. Absent versions (shouldn't happen for a real
	// paste) just skip the version stamp.
	verNum, verErr := r.legacyVersionNumForSHA(slug, sha)
	hasVer := verErr == nil

	return r.runAuthoritative(pinKey, refs, func(tx shaleKVTx, bind func() error) error {
		var head pasteRow
		if err := shaleTxGetJSON(tx, pasteKey, &head); err != nil {
			return err
		}
		if head.BlobID != ref.BlobID {
			head.BlobID = ref.BlobID
			if err := shaleTxPutJSON(tx, pasteKey, head); err != nil {
				return err
			}
		}
		if hasVer {
			verKey := shaleKeyVersion(slug, verNum)
			var ver versionRow
			if err := shaleTxGetJSON(tx, verKey, &ver); err != nil {
				return err
			}
			if ver.BlobID != ref.BlobID {
				ver.BlobID = ref.BlobID
				if err := shaleTxPutJSON(tx, verKey, ver); err != nil {
					return err
				}
			}
		}
		// Bind the pointer (idempotent tx.Put of the bref key) so it co-commits
		// even when the row already carried the id (a re-run after a lost bind).
		return bind()
	})
}

// rebindVersion sets BlobID on a single versions/<slug>/<NNNN> row + binds.
func (r *ShaleRepo) rebindVersion(slug domain.Slug, verNum int, ref cluster.BlobRef, pinKey []byte, refs []cluster.BlobRef) error {
	verKey := shaleKeyVersion(slug, verNum)
	return r.runAuthoritative(pinKey, refs, func(tx shaleKVTx, bind func() error) error {
		var ver versionRow
		if err := shaleTxGetJSON(tx, verKey, &ver); err != nil {
			return err
		}
		if ver.BlobID != ref.BlobID {
			ver.BlobID = ref.BlobID
			if err := shaleTxPutJSON(tx, verKey, ver); err != nil {
				return err
			}
		}
		return bind()
	})
}

// rebindSiteFile sets sites/<slug>'s FileBlobs[sha] = ref.BlobID + binds.
func (r *ShaleRepo) rebindSiteFile(slug domain.Slug, sha string, ref cluster.BlobRef, pinKey []byte, refs []cluster.BlobRef) error {
	siteKey := shaleKeySite(slug)
	return r.runAuthoritative(pinKey, refs, func(tx shaleKVTx, bind func() error) error {
		var sr siteRow
		if err := shaleTxGetJSON(tx, siteKey, &sr); err != nil {
			return err
		}
		if sr.FileBlobs == nil {
			sr.FileBlobs = make(map[string]string, 1)
		}
		if sr.FileBlobs[sha] != ref.BlobID {
			sr.FileBlobs[sha] = ref.BlobID
			if err := shaleTxPutJSON(tx, siteKey, sr); err != nil {
				return err
			}
		}
		return bind()
	})
}
