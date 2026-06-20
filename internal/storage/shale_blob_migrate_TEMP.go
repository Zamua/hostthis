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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
)

// LegacyBlobRecord names ONE legacy record-blob the migrator must re-key: a
// (slug, kind, contentSHA) triple plus whether the metadata already carries a
// shale blob id for it. The migrate loop stages the old <sha> bytes, then calls
// RebindLegacyBlob(slug, kind, contentSHA, ref); the dry-run/verify passes use
// the same triple. HasBlobID lets the loop's idempotency pre-check skip an
// already-rebound record BEFORE staging (so a re-run never mints a fresh blobid
// and leaks the newly-staged object).
type LegacyBlobRecord struct {
	Slug       domain.Slug
	Kind       LegacyBlobKind
	ContentSHA string
	HasBlobID  bool   // the row/side-table already carries a non-empty blob id for this sha
	BlobID     string // the existing blob id when HasBlobID (used by verify to read it back)
}

// ScanLegacyBlobs enumerates every record-blob across the cluster - one per
// paste head, one per non-deleted version row, one per distinct site-file sha -
// as the migrate/dry-run/verify input set. It REUSES the same cross-shard
// aggregatePrefix scan ReferencedBlobSHAs / ReferencedSiteBlobSHAs use (no
// re-implemented keyspace) and the existing pasteRow/versionRow/siteRow structs
// + decodeManifest, so the enumeration sees exactly the rows the read path
// resolves.
//
// Per record it reports HasBlobID = "the row/side-table already carries a
// non-empty blob id for this sha" so the migrate loop can skip an
// already-rebound record before staging (idempotency). A record with an EMPTY
// content sha is skipped (a paste with no body, never a blob); a deleted version
// is skipped (its blob is GC-eligible, the same exclusion ReferencedBlobSHAs
// applies). FAIL-CLOSED on any undecodable row: an under-enumerated set would
// silently leave a legacy blob un-migrated, so a decode error aborts the whole
// scan rather than skipping the row.
//
// The paste head and its serving version (the version whose ContentSHA == the
// head's) describe the SAME blob; this returns the head record (kind
// LegacyBlobPaste) for that sha and a per-version record only for versions whose
// sha differs from the head (a pinned / Show'd older version). RebindLegacyBlob
// for the head stamps BOTH the head and its serving version, mirroring
// insertAuthoritative; the per-version records cover the non-head versions.
func (r *ShaleRepo) ScanLegacyBlobs() ([]LegacyBlobRecord, error) {
	pasteItems, err := r.aggregatePrefix([]byte("pastes/"))
	if err != nil {
		return nil, fmt.Errorf("scan pastes: %w", err)
	}
	versionItems, err := r.aggregatePrefix([]byte("versions/"))
	if err != nil {
		return nil, fmt.Errorf("scan versions: %w", err)
	}
	siteItems, err := r.aggregatePrefix(prefixSites)
	if err != nil {
		return nil, fmt.Errorf("scan sites: %w", err)
	}

	var out []LegacyBlobRecord

	// Paste heads. headSHA[slug] = the head's served sha, so the version loop
	// can skip the serving version (the head record already covers that blob).
	headSHA := make(map[string]string, len(pasteItems))
	for _, item := range pasteItems {
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		slug := strings.TrimPrefix(string(item.Key), "pastes/")
		headSHA[slug] = p.ContentSHA
		if p.ContentSHA == "" {
			continue // bodyless paste: no blob to re-key
		}
		out = append(out, LegacyBlobRecord{
			Slug:       domain.Slug(slug),
			Kind:       LegacyBlobPaste,
			ContentSHA: p.ContentSHA,
			HasBlobID:  p.BlobID != "",
			BlobID:     p.BlobID,
		})
	}

	// Version rows. Skip the serving version (its sha == the head's; the head
	// record covers it) and tombstoned versions (GC-eligible, excluded exactly
	// as ReferencedBlobSHAs does). versions/<slug>/<NNNN>: trim "versions/" then
	// cut the trailing "/<NNNN>" to recover the slug (extractSlug would return
	// the NNNN segment, so parse explicitly here).
	for _, item := range versionItems {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		rest := strings.TrimPrefix(string(item.Key), "versions/")
		slug, _, ok := strings.Cut(rest, "/")
		if !ok || slug == "" {
			return nil, fmt.Errorf("scan: malformed version key %q", item.Key)
		}
		if v.Deleted || v.ContentSHA == "" {
			continue
		}
		if v.ContentSHA == headSHA[slug] {
			continue // the head record (or its rebind) covers the serving version
		}
		out = append(out, LegacyBlobRecord{
			Slug:       domain.Slug(slug),
			Kind:       LegacyBlobVersion,
			ContentSHA: v.ContentSHA,
			HasBlobID:  v.BlobID != "",
			BlobID:     v.BlobID,
		})
	}

	// Site files. One record per DISTINCT file sha in the manifest; HasBlobID
	// from the FileBlobs side-table.
	for _, item := range siteItems {
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		slug := strings.TrimPrefix(string(item.Key), "sites/")
		man, mErr := decodeManifest(row.Manifest)
		if mErr != nil {
			return nil, fmt.Errorf("decode manifest %s: %w", item.Key, mErr)
		}
		for _, sha := range man.SHASet() {
			if sha == "" {
				continue
			}
			existing := row.FileBlobs[sha]
			out = append(out, LegacyBlobRecord{
				Slug:       domain.Slug(slug),
				Kind:       LegacyBlobSiteFile,
				ContentSHA: sha,
				HasBlobID:  existing != "",
				BlobID:     existing,
			})
		}
	}

	return out, nil
}

// MemberCount returns the number of nodes in the cluster's current ring
// membership snapshot - the founder's readiness gate before it drives the R=2
// re-key loop. It wraps cluster.Members() (the same membership snapshot the
// rebalance + routing read). In single-node mode this is 1. The migrate binary
// polls it so the founder waits for the FULL ring (all migrate pods joined)
// before any RebindLegacyBlob, so every co-commit fans to both replica
// positions; with a one-member ring an R=2 write would land only one replica.
func (r *ShaleRepo) MemberCount() int {
	return len(r.cluster.Members())
}

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
