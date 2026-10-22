package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/Zamua/hostthis/internal/archive"
	"github.com/Zamua/hostthis/internal/domain"
)

// SiteRepo is the persistence interface the site-deploy + site-read
// services need. internal/storage.SiteRepo satisfies it.
type SiteRepo interface {
	InsertWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error
	// ReplaceWithQuotaCheck re-deploys an EXISTING owned site in place. s.Slug
	// names the target; s.Identity is the connecting key. A slug that is not a
	// site, and a site owned by another identity, both return
	// domain.ErrNotFound, so "not yours" is indistinguishable from "does not
	// exist". ErrOverUserQuota on quota overflow.
	ReplaceWithQuotaCheck(ctx context.Context, s domain.Site, storedBytes int, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Site, error)
	// Head reads the latest live version of an owned site and its manifest;
	// foreign and non-site slugs are domain.ErrNotFound.
	Head(slug domain.Slug, owner domain.Identity) (int, domain.Manifest, error)
	// Delete re-checks wantIdentity + wantCreatedAt inside its {slug}
	// transaction, so a delete+re-mint of the slug by another identity in the
	// window cannot destroy the new owner's paste. It answers with the upload id
	// of every version it removed.
	Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) (uploadIDs []string, err error)
	// SumActiveBytesByOwner returns the identity's active SITE bytes. The
	// deploy path adds the paste-side sum to compute the budget the untar may
	// fill before the persistence-time check.
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
	// ListSitesByOwner returns the identity's active sites. A site counts
	// against the shared quota, so `list` must show it or the quota is
	// invisible and unfreeable.
	ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error)
}

// PasteByteSummer is the slice of the paste repo the deploy path needs for
// its remaining-budget computation. internal/storage.PasteRepo satisfies it.
type PasteByteSummer interface {
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
}

// DeploySite is the application service for a static-site upload: safe-untar,
// one object per file under the deploy's own upload prefix, manifest,
// persisted Site. The per-identity quota is enforced BOTH mid-untar (the
// decompression-bomb guard) AND at persistence time (InsertWithQuotaCheck).
type DeploySite struct {
	Sites  SiteRepo
	Pastes PasteByteSummer
	Blob   BlobUnit
	Now    func() time.Time
	// Logger records object deletions that fail after the outcome is decided.
	// nil discards.
	Logger *log.Logger
	// MaxArchiveBytes caps the compressed archive read; 0 means
	// domain.MaxSiteArchiveBytes.
	MaxArchiveBytes int64
	// UploadIDs mints each upload's id, which decides where its bytes are
	// stored (docs/SPEC.md "Pod storage"). nil stores them in the bucket.
	UploadIDs UploadIDSource
}

// ErrSiteArchiveTooLarge is returned when the compressed archive itself
// exceeded the site archive cap, whatever it would have expanded to.
var ErrSiteArchiveTooLarge = fmt.Errorf("service: site archive too large to consider (input exceeded %d MiB cap)", domain.MaxSiteArchiveBytes>>20)

// SiteDeployer is the site surface the SSH layer consumes, an interface so
// cache invalidation can be a decorator the composition root layers on.
type SiteDeployer interface {
	Deploy(body io.Reader, owner string) (SiteResult, error)
	DeployToSlug(slug domain.Slug, body io.Reader, owner string) (SiteResult, error)
	Delete(slug domain.Slug, owner string) error
	ListSites(owner string) ([]domain.Site, error)
	Have(slug domain.Slug, owner string) (Held, error)
}

// Held is a site head as a keep list: its version and each file's digest,
// for the files a keep list can name.
type Held struct {
	Base  int
	Files []HeldFile
}

// HeldFile is one keepable file of a site head.
type HeldFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// Have reads an owned site's head as a keep list (docs/SPEC.md "Redeploy
// reuse"), sorted by path. A foreign or non-site slug is ErrNotFound.
func (d *DeploySite) Have(slug domain.Slug, owner string) (Held, error) {
	if owner == "" {
		return Held{}, ErrEmptyOwner
	}
	ver, man, err := d.Sites.Head(slug, domain.Identity(owner))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Held{}, ErrNotFound
		}
		return Held{}, err
	}
	held := Held{Base: ver, Files: []HeldFile{}}
	for _, p := range slices.Sorted(maps.Keys(man.Files)) {
		if e := man.Files[p]; e.Digest != "" && domain.KeepablePath(p) {
			held.Files = append(held.Files, HeldFile{Path: p, Digest: e.Digest})
		}
	}
	return held, nil
}

var _ SiteDeployer = (*DeploySite)(nil)

// NewDeploySite wires defaults.
func NewDeploySite(sites SiteRepo, pastes PasteByteSummer, blob BlobUnit) *DeploySite {
	return &DeploySite{Sites: sites, Pastes: pastes, Blob: blob, Now: time.Now}
}

// ListSites returns the owner's active static sites. Anonymous / empty owners
// get an empty list: only keyed identities own content.
func (d *DeploySite) ListSites(owner string) ([]domain.Site, error) {
	if !domain.Identity(owner).IsKeyed() {
		return nil, nil
	}
	return d.Sites.ListSitesByOwner(owner, d.Now().UTC())
}

// SiteResult is what Deploy produced for the SSH layer to format.
type SiteResult struct {
	Site domain.Site
}

// ErrEmptySite is returned when an archive safe-untars to zero files.
var ErrEmptySite = errors.New("service: archive contains no files")

// maxDeployRetries bounds the slug reservation retry budget.
const maxDeployRetries = 5

// Deploy reads a gzip-tar archive from body, untars it safely, stores the
// files, and persists a new Site owned by owner. body is consumed once,
// mid-untar, so peak memory is one file at a time, never the inflated archive.
//
// Returns:
//   - domain.ErrUnsupportedKind: not a valid gzip-tar
//   - ErrOverQuota: the archive expands past the owner's remaining quota,
//     caught mid-untar by the decompression-bomb guard or at persistence time
//     by the atomic check
//   - ErrTooManyFiles-family errors surfaced verbatim via domain
func (d *DeploySite) Deploy(body io.Reader, owner string) (SiteResult, error) {
	if owner == "" {
		return SiteResult{}, ErrEmptyOwner
	}
	now := d.Now().UTC()
	man, uploadID, err := d.extract(body, owner, now, nil)
	if err != nil {
		return SiteResult{}, err
	}

	site := domain.Site{
		Identity: domain.Identity(owner),
		// The shape is decided here, from THIS deploy's manifest, and stored on
		// the row; serving never re-derives it.
		Kind:      man.ArchiveKind(),
		UploadID:  uploadID,
		Manifest:  man,
		CreatedAt: now,
		UpdatedAt: now,
	}
	stored := man.CompressedSize()

	// Objects are keyed by upload, not slug, so a collision retries only the
	// metadata insert. The one-shot archive never needs to be read again.
	for range maxDeployRetries {
		site.Slug = domain.NewRandomSlug()
		err := d.Sites.InsertWithQuotaCheck(context.Background(), site, stored, int64(domain.UserQuotaBytes), now)
		if errors.Is(err, domain.ErrSlugTaken) {
			continue
		}
		if err != nil {
			abandonUpload(d.Blob, d.Logger, uploadID, err)
			_, terr := classifyCommitErr(err)
			return SiteResult{}, terr
		}
		return SiteResult{Site: site}, nil
	}
	discardUpload(d.Blob, d.Logger, uploadID)
	return SiteResult{}, ErrSlugTaken
}

// extract safe-untars body into a fresh upload under the owner's remaining
// budget and returns the manifest and that upload's id. The decompression-bomb
// guard aborts the instant the running total would cross that budget, so a
// site can never be extracted over-quota. Other sink errors pass through the
// classifier; ErrUnsafeArchive / ErrTooManyFiles surface verbatim so the
// SSH layer can message them precisely. A failed extract deletes whatever it
// staged.
func (d *DeploySite) extract(body io.Reader, owner string, now time.Time, base *siteBase) (domain.Manifest, string, error) {
	usedPaste, err := d.Pastes.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return domain.Manifest{}, "", fmt.Errorf("sum paste bytes: %w", err)
	}
	usedSite, err := d.Sites.SumActiveBytesByOwner(owner, now)
	if err != nil {
		return domain.Manifest{}, "", fmt.Errorf("sum site bytes: %w", err)
	}
	budget := siteExtractBudget(int64(domain.UserQuotaBytes), int64(usedPaste), usedSite)

	uploadID, err := d.UploadIDs.next(owner)
	if err != nil {
		return domain.Manifest{}, "", fmt.Errorf("upload id: %w", err)
	}
	sink := &blobSink{blob: d.Blob, uploadID: uploadID, base: base.standalone()}
	defer sink.Close()
	limit := d.MaxArchiveBytes
	if limit <= 0 {
		limit = domain.MaxSiteArchiveBytes
	}
	capped := &archiveCapReader{r: body, left: limit}
	man, keep, err := archive.UntarKeep(capped, sink, budget)
	if err == nil && keep != nil {
		err = d.applyKeepList(man, *keep, base, sink)
	}
	if err == nil {
		err = sink.Flush()
	}
	switch {
	// First: the codec reports a cut-off stream as whatever it was decoding.
	case err != nil && capped.hit:
		err = ErrSiteArchiveTooLarge
	case errors.Is(err, domain.ErrArchiveTooLarge):
		err = ErrOverQuota
	case errors.Is(err, domain.ErrUnsupportedKind):
		err = domain.ErrUnsupportedKind
	case errors.Is(err, domain.ErrBadKeepList), errors.Is(err, domain.ErrBaseMoved):
	case err != nil:
		_, err = classifyCommitErr(err)
	case len(man.Files) == 0:
		err = ErrEmptySite
	}
	if err != nil {
		discardUpload(d.Blob, d.Logger, uploadID)
		return domain.Manifest{}, "", err
	}
	return man, uploadID, nil
}

// archiveCapReader passes at most left bytes and fails, rather than ending the
// stream, once r has more.
type archiveCapReader struct {
	r    io.Reader
	left int64
	hit  bool
}

var errArchiveCap = errors.New("site archive cap exceeded")

func (c *archiveCapReader) Read(p []byte) (int, error) {
	if c.hit {
		return 0, errArchiveCap
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.left {
		c.hit = true
		return int(c.left), errArchiveCap
	}
	c.left -= int64(n)
	return n, err
}

// Delete removes an owned static site by slug, then deletes every removed
// version's bytes. A non-site slug and a foreign-owned site both collapse to
// ErrNotFound, the same sentinel the paste-delete path returns, so existence
// and ownership never leak.
func (d *DeploySite) Delete(slug domain.Slug, owner string) error {
	if owner == "" {
		return ErrEmptyOwner
	}
	existing, err := d.Sites.Get(slug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("get site: %w", err)
	}
	if existing.Identity.String() != owner {
		return ErrNotFound
	}
	uploads, err := d.Sites.Delete(slug, existing.Identity, existing.CreatedAt)
	if err != nil {
		return err
	}
	for _, id := range uploads {
		discardUpload(d.Blob, d.Logger, id)
	}
	return nil
}

// DeployToSlug appends a SITE version at an existing owned slug. Same pipeline
// as Deploy, but it targets slug instead of minting one and charges the complete
// retained version.
//
// The untar budget is the owner's remaining allowance. Prior versions remain
// retained and charged, so a targeted redeploy receives no replacement credit.
// Returns:
//   - ErrEmptyOwner: anonymous / empty identity
//   - ErrNotFound: slug is not a site owned by owner
//   - domain.ErrUnsupportedKind: not a valid gzip-tar
//   - ErrOverQuota: over the per-identity cap
//   - ErrEmptySite: the archive safe-untars to zero files
func (d *DeploySite) DeployToSlug(slug domain.Slug, body io.Reader, owner string) (SiteResult, error) {
	if owner == "" {
		return SiteResult{}, ErrEmptyOwner
	}
	now := d.Now().UTC()

	// Verify ownership BEFORE reading the body.
	existing, err := d.Sites.Get(slug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return SiteResult{}, ErrNotFound
		}
		return SiteResult{}, fmt.Errorf("get site: %w", err)
	}
	if existing.Identity.String() != owner {
		return SiteResult{}, ErrNotFound
	}

	baseVer, baseMan, err := d.Sites.Head(slug, domain.Identity(owner))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return SiteResult{}, ErrNotFound
		}
		return SiteResult{}, fmt.Errorf("read site head: %w", err)
	}
	base := &siteBase{ver: baseVer, man: baseMan}

	man, uploadID, err := d.extract(body, owner, now, base)
	if err != nil {
		return SiteResult{}, err
	}

	site := domain.Site{
		Base:     baseVer,
		Slug:     slug,
		Identity: domain.Identity(owner),
		// A redeploy re-decides the shape from its own manifest, so adding a
		// root index.html turns a knowledge base into a site at the same slug.
		Kind:      man.ArchiveKind(),
		UploadID:  uploadID,
		Manifest:  man,
		CreatedAt: existing.CreatedAt, // preserved across re-deploys
		UpdatedAt: now,
	}
	err = d.Sites.ReplaceWithQuotaCheck(context.Background(), site, man.CompressedSize(), int64(domain.UserQuotaBytes), now)
	if err != nil {
		abandonUpload(d.Blob, d.Logger, uploadID, err)
	}
	switch class, terr := classifyCommitErr(err); {
	case class == commitOK:
		return SiteResult{Site: site}, nil
	case errors.Is(err, domain.ErrNotFound):
		// Deleted between the ownership check and the swap.
		// Same shape the up-front check would have yielded.
		return SiteResult{}, ErrNotFound
	default:
		return SiteResult{}, terr
	}
}

// blobSink stores an archive's files as objects of one upload: small files
// into packs, others alone. A path the archive repeats leaves its earlier
// bytes unreferenced in that same prefix, which the upload's delete still
// covers.
type blobSink struct {
	blob     BlobUnit
	uploadID string
	// base maps the digest of each standalone file of the version this deploy
	// is computed against to its entry; a file matching one is not stored.
	base    map[string]domain.ManifestEntry
	next    int
	pack    domain.BlobPack
	packKey string
}

// siteExtractBudget is the unallocated owner quota available to the next
// retained manifest version.
func siteExtractBudget(cap, usedPaste, usedSite int64) int64 {
	return domain.Allowance{Cap: cap, Used: usedPaste + usedSite}.Remaining()
}

func (s *blobSink) nextKey() string {
	key := domain.UploadObjectKey(s.uploadID, s.next)
	s.next++
	return key
}

func (s *blobSink) Store(p string, r io.Reader, size int64) (domain.BlobRef, int, error) {
	if domain.Packable(size) {
		return s.storePacked(p, r)
	}
	if d, ok := r.(*domain.DigestReader); ok && len(s.base) > 0 {
		return s.storeUnlessHeld(p, d)
	}
	key := s.nextKey()
	// No buffer: the body streams through the compressor into the object store,
	// so an in-flight file costs the compressor window and a copy buffer rather
	// than its own size. The untar guard admits a single file up to the owner's
	// whole remaining allowance, so buffering would size peak memory to the
	// QUOTA.
	compressedSize, err := s.blob.StageEncoding(context.Background(), key, r)
	if err != nil {
		return domain.BlobRef{}, 0, storeErr(p, err)
	}
	return domain.BlobRef{Key: key}, compressedSize, nil
}

// storeUnlessHeld spills the file to disk while hashing it, then points at the
// base's object when that holds the same bytes, and stores the spill
// otherwise. Disk, not memory, is the buffer.
func (s *blobSink) storeUnlessHeld(p string, r *domain.DigestReader) (domain.BlobRef, int, error) {
	spill, err := os.CreateTemp(os.Getenv("HOSTTHIS_STAGING_DIR"), "hostthis-file-*")
	if err != nil {
		return domain.BlobRef{}, 0, fmt.Errorf("create file spill: %w", err)
	}
	defer os.Remove(spill.Name()) //nolint:errcheck
	defer spill.Close()           //nolint:errcheck
	if _, err := io.Copy(spill, r); err != nil {
		return domain.BlobRef{}, 0, storeErr(p, err)
	}
	if held, ok := s.base[r.Digest()]; ok {
		return held.Ref(), held.CompressedSize, nil
	}
	if _, err := spill.Seek(0, io.SeekStart); err != nil {
		return domain.BlobRef{}, 0, fmt.Errorf("rewind file spill: %w", err)
	}
	key := s.nextKey()
	compressedSize, err := s.blob.StageEncoding(context.Background(), key, spill)
	if err != nil {
		return domain.BlobRef{}, 0, storeErr(p, err)
	}
	return domain.BlobRef{Key: key}, compressedSize, nil
}

func (s *blobSink) storePacked(p string, r io.Reader) (domain.BlobRef, int, error) {
	return s.intoPack(p, func(pack domain.BlobPack) (int64, int64, error) { return pack.Append(r) })
}

// appendStored adds an already-encoded member to the open pack.
func (s *blobSink) appendStored(p string, r io.Reader) (domain.BlobRef, int, error) {
	return s.intoPack(p, func(pack domain.BlobPack) (int64, int64, error) { return pack.AppendStored(r) })
}

func (s *blobSink) intoPack(p string, add func(domain.BlobPack) (int64, int64, error)) (domain.BlobRef, int, error) {
	if s.pack == nil {
		key := s.nextKey()
		pack, err := s.blob.OpenPack(key)
		if err != nil {
			return domain.BlobRef{}, 0, fmt.Errorf("open pack: %w", err)
		}
		s.pack, s.packKey = pack, key
	}
	off, n, err := add(s.pack)
	if err != nil {
		return domain.BlobRef{}, 0, storeErr(p, err)
	}
	ref := domain.BlobRef{Key: s.packKey, Offset: off, Length: n}
	if domain.PackFull(s.pack.Len()) {
		if err := s.Flush(); err != nil {
			return domain.BlobRef{}, 0, err
		}
	}
	return ref, int(n), nil
}

// Flush writes the open pack, if any. Every reference a manifest holds is
// durable once the last Flush returns.
func (s *blobSink) Flush() error {
	if s.pack == nil {
		return nil
	}
	err := s.pack.Commit(context.Background())
	_ = s.pack.Close()
	s.pack = nil
	if err != nil {
		return fmt.Errorf("blob put pack: %w", err)
	}
	return nil
}

// Close drops an unwritten pack's local state.
func (s *blobSink) Close() {
	if s.pack != nil {
		_ = s.pack.Close()
		s.pack = nil
	}
}

// siteBase is the version a redeploy is computed against.
type siteBase struct {
	ver int
	man domain.Manifest
}

// standalone indexes the base's unpacked files by digest. Packs are never
// shared, so a packed file is not offered.
func (b *siteBase) standalone() map[string]domain.ManifestEntry {
	if b == nil {
		return nil
	}
	out := map[string]domain.ManifestEntry{}
	for _, e := range b.man.Files {
		if e.Digest != "" && e.Key != "" && e.Length == 0 {
			out[e.Digest] = e
		}
	}
	return out
}

// applyKeepList carries each listed base file into man: a standalone file by
// reference, a packed one copied into this deploy's pack. Any mismatch
// refuses the whole deploy (docs/SPEC.md "Redeploy reuse").
func (d *DeploySite) applyKeepList(man domain.Manifest, keep domain.KeepList, base *siteBase, sink *blobSink) error {
	if base == nil {
		return fmt.Errorf("%w: a new site has no base to keep from", domain.ErrBadKeepList)
	}
	if keep.Base != base.ver {
		return fmt.Errorf("%w: it was made for version %d, the latest is %d; run have again",
			domain.ErrBadKeepList, keep.Base, base.ver)
	}
	packed := map[string][]keptMember{}
	for _, p := range slices.Sorted(maps.Keys(keep.Paths)) {
		if _, sent := man.Files[p]; sent {
			return fmt.Errorf("%w: %s is both listed and sent", domain.ErrBadKeepList, p)
		}
		held, ok := base.man.Files[p]
		if !ok || held.Digest == "" || held.Digest != keep.Paths[p] {
			return fmt.Errorf("%w: %s does not match version %d", domain.ErrBadKeepList, p, base.ver)
		}
		if held.Length == 0 {
			man.Add(p, held)
			continue
		}
		packed[held.Key] = append(packed[held.Key], keptMember{path: p, entry: held})
	}
	for _, key := range slices.Sorted(maps.Keys(packed)) {
		if err := d.copyMembers(key, packed[key], man, sink); err != nil {
			return err
		}
	}
	return nil
}

type keptMember struct {
	path  string
	entry domain.ManifestEntry
}

// copyMembers reads one base pack once, in offset order, and moves each kept
// member's stored bytes into this deploy's pack unchanged.
func (d *DeploySite) copyMembers(key string, members []keptMember, man domain.Manifest, sink *blobSink) error {
	slices.SortFunc(members, func(a, b keptMember) int { return cmp.Compare(a.entry.Offset, b.entry.Offset) })
	rc, err := d.Blob.ReadStored(context.Background(), key)
	if err != nil {
		return fmt.Errorf("read kept pack %q: %w", key, err)
	}
	defer rc.Close() //nolint:errcheck
	var pos int64
	for _, m := range members {
		if _, err := io.CopyN(io.Discard, rc, m.entry.Offset-pos); err != nil {
			return fmt.Errorf("read kept pack %q: %w", key, err)
		}
		ref, n, err := sink.appendStored(m.path, io.LimitReader(rc, m.entry.Length))
		if err != nil {
			return err
		}
		if n != int(m.entry.Length) {
			return fmt.Errorf("read kept pack %q: member at %d is short", key, m.entry.Offset)
		}
		pos = m.entry.Offset + m.entry.Length
		e := m.entry
		e.Key, e.Offset, e.Length, e.CompressedSize = ref.Key, ref.Offset, ref.Length, n
		man.Add(m.path, e)
	}
	return nil
}

// storeErr keeps the untar's cap sentinel intact, so SafeUntar can tell a
// too-large archive from a real I/O failure.
func storeErr(p string, err error) error {
	if errors.Is(err, domain.ErrArchiveTooLarge) {
		return domain.ErrArchiveTooLarge
	}
	return fmt.Errorf("blob put %q: %w", p, err)
}

// ArchiveAdapter presents DeploySite as the upload service's ArchiveDeployer,
// so one Create call handles both cardinalities and no transport has to know
// there are two services behind it.
type ArchiveAdapter struct{ Deployer *DeploySite }

func (a ArchiveAdapter) Deploy(body io.Reader, owner string) (Result, error) {
	res, err := a.Deployer.Deploy(body, owner)
	if err != nil {
		return Result{}, err
	}
	// Only the slug and owner are read downstream; the manifest stays on the
	// stored paste rather than being copied into the response.
	return Result{Paste: domain.Paste{
		Slug:      res.Site.Slug,
		Identity:  res.Site.Identity,
		Kind:      res.Site.Kind,
		CreatedAt: res.Site.CreatedAt,
		UpdatedAt: res.Site.UpdatedAt,
	}}, nil
}
