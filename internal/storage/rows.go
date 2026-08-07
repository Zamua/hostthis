// The stored row shapes: how a paste, a version, a site and a room are
// serialized into the KV plane, and how they come back as domain values.
//
// Engine-independent by construction. These are the on-disk contract every
// metadata backend writes and reads, so a row written by one is readable by
// the next - which is what makes swapping the engine underneath a build-time
// choice rather than a migration.

package storage

import (
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// contentRef is the served-content descriptor: the four fields that together
// name the stored bytes a paste version points at. They travel as ONE value (a
// version row carries its own, a paste head carries the SERVED version's) so
// "make version V the served one" is a single whole-value assignment and no
// field can be repointed without the others. Moving ContentSHA while leaving
// BlobID behind would make the head serve the wrong blob under value
// separation, where the read seam resolves bytes by BlobID.
//
// Embedded ANONYMOUSLY in pasteRow and versionRow so the four keys stay at the
// top level of the JSON.
type contentRef struct {
	Kind       string `json:"kind"`
	ContentSHA string `json:"content_sha"`
	BlobID     string `json:"blob_id,omitempty"` // shale-blob path: the staged blob id GetBlob needs; "" on the standalone sha-keyed path
	Size       int    `json:"size"`
}

type pasteRow struct {
	Identity      string    `json:"identity"`
	Status        string    `json:"status,omitempty"` // pending|ready|failed; "" reads as ready
	contentRef              // the SERVED version's descriptor, promoted to flat JSON
	Name          string    `json:"name"`
	PinnedVersion int       `json:"pinned_version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// LiveBytes and LatestVersion are the paste's totals, maintained in the
	// SAME {slug} transaction that writes or tombstones a version row. The head
	// and its versions co-shard, so these are transactionally exact rather than
	// derived figures that can drift.
	//
	// They exist so a reader that already holds the head does not also have to
	// prefix-scan the version family: `list` is one routed read per item, and
	// the enumeration entry's cached size is verified against LiveBytes rather
	// than against a recomputation.
	//
	// Zero on a row written before this field existed. Readers treat a zero
	// LatestVersion as "unknown" and fall back to scanning, so an old row keeps
	// working until its next write refreshes it.
	LiveBytes     int `json:"live_bytes,omitempty"`
	LatestVersion int `json:"latest_version,omitempty"`
}

type versionRow struct {
	VerNum     int       `json:"ver_num"`
	contentRef           // this version's descriptor
	CreatedAt  time.Time `json:"created_at"`
	Deleted    bool      `json:"deleted"`
}

func (p pasteRow) toDomain(slug domain.Slug) domain.Paste {
	return domain.Paste{
		Slug:          slug,
		Identity:      domain.Identity(p.Identity),
		Status:        domain.NormalizeStatus(p.Status),
		Kind:          domain.ContentKind(p.Kind),
		ContentSHA:    p.ContentSHA,
		Size:          p.Size,
		Name:          p.Name,
		PinnedVersion: p.PinnedVersion,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
	}
}

func pasteFromDomain(p domain.Paste) pasteRow {
	return pasteRow{
		Identity: p.Identity.String(),
		Status:   string(domain.NormalizeStatus(string(p.Status))),
		// domain.Paste carries no BlobID (a storage/value-separation
		// detail); the insert path sets the head's BlobID after this.
		contentRef:    contentRef{Kind: string(p.Kind), ContentSHA: p.ContentSHA, Size: p.Size},
		Name:          p.Name,
		PinnedVersion: p.PinnedVersion,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
	}
}

func (v versionRow) toDomain(slug domain.Slug) domain.Version {
	return domain.Version{
		Slug:       slug,
		VerNum:     v.VerNum,
		Kind:       domain.ContentKind(v.Kind),
		ContentSHA: v.ContentSHA,
		Size:       v.Size,
		CreatedAt:  v.CreatedAt,
		Deleted:    v.Deleted,
	}
}

type scanItem struct {
	Key   []byte
	Value []byte
}

type siteRow struct {
	Identity    string    `json:"identity"`
	Manifest    string    `json:"manifest"`
	DedupedSize int       `json:"deduped_size"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// FileBlobs maps a file's content sha to the shale-blob id its bytes were
	// staged under. The manifest references files by sha, so this side-table is
	// how the read path resolves sha -> blobid for GetBlob. Empty on the
	// standalone paths (sqlite / slatedb / disk), where a file is
	// content-addressed by sha alone.
	FileBlobs map[string]string `json:"file_blobs,omitempty"`
}

type roomRow struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ByteTotal int64     `json:"byte_total,omitempty"` // shale-only running per-room byte total
	KeyCount  int       `json:"key_count,omitempty"`  // shale-only running per-room key count
	// Seq is the per-room mutation sequence: dense, +1 per committed
	// PUT/DELETE, bumped by the touch every mutation performs on this record.
	// Maintained by both the slatedb and shale backends. A missing field
	// reads as 0. See SPEC "The per-room sequence: assignment at commit".
	Seq uint64 `json:"seq,omitempty"`
}

func roomRowFromDomain(r domain.Room) roomRow {
	return roomRow{CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (row roomRow) toDomain(appSlug domain.Slug, id domain.RoomID) domain.Room {
	return domain.Room{
		AppSlug:   appSlug,
		ID:        id,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// extractSlug takes the segment after the last '/', the slug in
// "identity_pastes/<identity>/<slug>".
func extractSlug(key []byte) string {
	s := string(key)
	idx := strings.LastIndex(s, "/")
	if idx < 0 {
		return s
	}
	return s[idx+1:]
}

func sortByUpdatedAtDesc(ps []domain.Paste) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].UpdatedAt.After(ps[j-1].UpdatedAt); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

func sortVersionsDesc(vs []domain.Version) {
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && vs[j].VerNum > vs[j-1].VerNum; j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
}

func (row siteRow) toDomain(slug domain.Slug) (domain.Site, error) {
	man, err := decodeManifest(row.Manifest)
	if err != nil {
		return domain.Site{}, err
	}
	return domain.Site{
		Slug:        slug,
		Identity:    domain.Identity(row.Identity),
		Manifest:    man,
		StoredBytes: row.DedupedSize,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}, nil
}

// splitRoomCreateRest parses "<subnet>/<ts>/<uuid>" (a roomcreate key with the
// "roomcreate/<app-slug>/" prefix stripped) into (subnet, ts). The subnet
// itself contains a '/' (e.g. "1.2.3.0/24"), so the split works from the RIGHT,
// peeling the two trailing slash-free segments.
func splitRoomCreateRest(rest string) (subnet, ts string, ok bool) {
	lastSlash := strings.LastIndex(rest, "/")
	if lastSlash < 0 {
		return "", "", false
	}
	beforeUUID := rest[:lastSlash] // "<subnet>/<ts>"
	tsSlash := strings.LastIndex(beforeUUID, "/")
	if tsSlash < 0 {
		return "", "", false
	}
	return beforeUUID[:tsSlash], beforeUUID[tsSlash+1:], true
}
