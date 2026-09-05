package storage

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// MemRepo is the in-process metadata plane: every port the services consume,
// over plain maps under one mutex.
//
// It exists for two consumers. Tests get a fast, dependency-free repo that the
// SAME conformance suite the celld backend passes keeps honest - an assertion
// that passes here and fails there is a backend bug, not a fixture artifact.
// Dev gets `HOSTTHIS_METADATA_BACKEND=memory`, the zero-setup `make run` path;
// it is EPHEMERAL by design and says so at startup.
//
// One mutex, deliberately. Every check-then-write (quota, caps, keygate
// windows) is atomic under it, which makes this the STRICTEST backend:
// anything admitted concurrently here is admissible everywhere.
type MemRepo struct {
	mu sync.Mutex

	pastes map[domain.Slug]*memPaste
	owners map[string]*memOwner

	keygate map[string]map[string]time.Time // subnet -> identity -> firstSeen

	rooms      map[memRoomKey]*memRoom
	roomBytes  map[domain.Slug]int64
	roomLedger map[domain.Slug][]memRoomCreate
	pushKeys   map[domain.Slug]string // app -> VAPID public key, base64url
}

type memPaste struct {
	row      domain.Paste
	versions []domain.Version // ascending VerNum; tombstones retained
	maxVer   int
}

type memOwner struct {
	firstSeen time.Time
}

func NewMemRepo() *MemRepo {
	return &MemRepo{
		pastes:     make(map[domain.Slug]*memPaste),
		owners:     make(map[string]*memOwner),
		keygate:    make(map[string]map[string]time.Time),
		rooms:      make(map[memRoomKey]*memRoom),
		roomBytes:  make(map[domain.Slug]int64),
		roomLedger: make(map[domain.Slug][]memRoomCreate),
		pushKeys:   make(map[domain.Slug]string),
	}
}

// --- internal helpers (mu held) ---------------------------------------------

// liveVersions returns the non-tombstoned versions, ascending.
func (p *memPaste) liveVersions() []domain.Version {
	out := make([]domain.Version, 0, len(p.versions))
	for _, v := range p.versions {
		if !v.Deleted {
			out = append(out, v)
		}
	}
	return out
}

func (p *memPaste) chargedBytes() int {
	total := 0
	for _, v := range p.liveVersions() {
		total += v.Size
	}
	return total
}

// live, otherwise the newest live version.
func (p *memPaste) served() (domain.Version, bool) {
	live := p.liveVersions()
	if len(live) == 0 {
		return domain.Version{}, false
	}
	if p.row.PinnedVersion != 0 {
		for _, v := range live {
			if v.VerNum == p.row.PinnedVersion {
				return v, true
			}
		}
	}
	return live[len(live)-1], true
}

// rollServed mirrors the served version's display fields onto the row. The
// row's kind, sha, manifest and size are a VIEW of the served version; every
// mutation that can change which version is served passes through here.
func (p *memPaste) rollServed() {
	v, ok := p.served()
	if !ok {
		return
	}
	p.row.Kind = v.Kind
	p.row.ContentSHA = v.ContentSHA
	p.row.Manifest = v.Manifest
	p.row.Size = v.Size
	p.row.LatestVersion = p.maxVer
}

// chargedBytes is one owner's quota charge: the sum of every LIVE version's
// size across their non-failed pastes. Distinct from any row's Size, which is
// the served version's alone.
func (r *MemRepo) chargedBytes(owner string) int {
	total := 0
	for _, p := range r.pastes {
		if p.row.Identity.String() != owner || p.row.Status == domain.PasteStatusFailed {
			continue
		}
		for _, v := range p.liveVersions() {
			total += v.Size
		}
	}
	return total
}

func (r *MemRepo) noteOwner(owner string, now time.Time) {
	if _, ok := r.owners[owner]; !ok {
		r.owners[owner] = &memOwner{firstSeen: now}
	}
}

// --- PasteRepo ---------------------------------------------------------------

func (r *MemRepo) InsertWithQuotaCheck(_ context.Context, p domain.Paste, userCap int64, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.pastes[p.Slug]; exists {
		return domain.ErrSlugTaken
	}
	if userCap > 0 && int64(r.chargedBytes(p.Identity.String())+p.Size) > userCap {
		return domain.ErrOverUserQuota
	}
	if p.Generation == "" {
		p.Generation = domain.NewPasteGeneration()
	}
	if p.LatestVersion == 0 {
		p.LatestVersion = 1
	}
	mp := &memPaste{row: p, maxVer: 1}
	// v1 is SEEDED into the version list: one list holds every version, so no
	// reader special-cases the first.
	mp.versions = append(mp.versions, domain.Version{
		Slug: p.Slug, VerNum: 1, Kind: p.Kind, ContentSHA: p.ContentSHA,
		Size: p.Size, CreatedAt: p.CreatedAt, Manifest: p.Manifest,
	})
	r.pastes[p.Slug] = mp
	r.noteOwner(p.Identity.String(), now)
	return nil
}

func (r *MemRepo) Get(slug domain.Slug) (domain.Paste, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok {
		return domain.Paste{}, ErrNotFound
	}
	return p.row, nil
}

func (r *MemRepo) MarkReady(paste domain.Paste) error {
	return r.setStatus(paste, domain.PasteStatusReady)
}
func (r *MemRepo) MarkFailed(paste domain.Paste) error {
	return r.setStatus(paste, domain.PasteStatusFailed)
}

// setStatus advances a PENDING paste and nothing else. Ready and failed are
// TERMINAL: a late finalizer racing the reconciler must not resurrect a failed
// paste or fail a served one, and an absent slug is a no-op for the same
// late-racer reason.
func (r *MemRepo) setStatus(paste domain.Paste, st domain.PasteStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pastes[paste.Slug]; ok && p.row.Status == domain.PasteStatusPending &&
		p.row.Generation == paste.Generation {
		p.row.Status = st
	}
	return nil
}

// --- PasteAdmin --------------------------------------------------------------

// ListByOwner: the owner's non-failed pastes, most recently updated first.
func (r *MemRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Paste
	for _, p := range r.pastes {
		if p.row.Identity.String() == owner && p.row.Status != domain.PasteStatusFailed {
			row := p.row
			row.StoredBytes = p.chargedBytes()
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (r *MemRepo) SumActiveBytesByOwner(owner string, _ time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chargedBytes(owner), nil
}

func (r *MemRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if o, ok := r.owners[owner]; ok {
		return o.firstSeen, nil
	}
	return time.Time{}, nil
}

func (r *MemRepo) OwnerSummary(owner string, _ time.Time) (domain.OwnerSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := domain.OwnerSummary{PasteBytes: int64(r.chargedBytes(owner))}
	for _, p := range r.pastes {
		if p.row.Identity.String() == owner && p.row.Status != domain.PasteStatusFailed {
			sum.Active++
		}
	}
	if o, ok := r.owners[owner]; ok {
		sum.FirstSeen = o.firstSeen
	}
	return sum, nil
}

// DropStaleOwnerEntry repairs an index entry whose paste is gone. One map
// holds row and index here, so a stale entry CANNOT exist and there is never
// anything to drop - the honest answer is always false, which is also what
// protects a live paste from a mistaken repair.
func (r *MemRepo) DropStaleOwnerEntry(domain.Slug, string) (bool, error) {
	return false, nil
}

// Delete removes a paste, guarded by owner and creation time so a delete
// cannot land on a slug re-minted by someone else in between.
func (r *MemRepo) Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok {
		return ErrNotFound
	}
	if p.row.Identity != wantIdentity || !p.row.CreatedAt.Equal(wantCreatedAt) {
		return ErrNotFound
	}
	delete(r.pastes, slug)
	return nil
}

func (r *MemRepo) SetName(slug domain.Slug, name string, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok || p.row.Identity != wantIdentity || !p.row.CreatedAt.Equal(wantCreatedAt) {
		return ErrNotFound
	}
	p.row.Name = name
	return nil
}

func (r *MemRepo) SetPinnedVersion(slug domain.Slug, generation string, v domain.Version) error {
	return r.setPin(slug, generation, v.VerNum)
}

func (r *MemRepo) Unpin(slug domain.Slug, generation string) error {
	return r.setPin(slug, generation, 0)
}

func (r *MemRepo) setPin(slug domain.Slug, generation string, ver int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok || p.row.Generation != generation {
		return ErrNotFound
	}
	p.row.PinnedVersion = ver
	p.rollServed()
	return nil
}

func (r *MemRepo) AppendVersionWithQuotaCheck(_ context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time,
) (domain.AppendResult, error) {
	return r.appendVersion(slug, generation, kind, contentSHA, size, domain.Manifest{}, userCap, now)
}

func (r *MemRepo) AppendManifestVersion(_ context.Context, slug domain.Slug, generation string,
	m domain.Manifest, root domain.ManifestEntry, size int, userCap int64, now time.Time,
) (AppendResult, error) {
	return r.appendVersion(slug, generation, domain.KindSite, root.SHA, size, m, userCap, now)
}

func (r *MemRepo) appendVersion(slug domain.Slug, generation string, kind domain.ContentKind, contentSHA string,
	size int, m domain.Manifest, userCap int64, now time.Time,
) (domain.AppendResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok || p.row.Generation != generation {
		return domain.AppendResult{}, ErrNotFound
	}
	if userCap > 0 && int64(r.chargedBytes(p.row.Identity.String())+size) > userCap {
		return domain.AppendResult{}, domain.ErrOverUserQuota
	}
	// A retired number is never reused, which is why maxVer is stored rather
	// than derived from the (tombstone-holding) list.
	p.maxVer++
	p.versions = append(p.versions, domain.Version{
		Slug: slug, VerNum: p.maxVer, Kind: kind, ContentSHA: contentSHA,
		Size: size, CreatedAt: now, Manifest: m,
	})
	wasPinned := p.row.PinnedVersion != 0
	p.row.UpdatedAt = now
	p.rollServed()
	return domain.AppendResult{NewVer: p.maxVer, WasPinned: wasPinned}, nil
}

// ListVersions: every version, tombstones included, NEWEST FIRST.
func (r *MemRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]domain.Version, len(p.versions))
	copy(out, p.versions)
	sort.Slice(out, func(i, j int) bool { return out[i].VerNum > out[j].VerNum })
	return out, nil
}

func (r *MemRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok {
		return domain.Version{}, ErrNotFound
	}
	for _, v := range p.versions {
		if v.VerNum == ver {
			return v, nil
		}
	}
	return domain.Version{}, ErrNotFound
}

func (r *MemRepo) IsVersionServed(slug domain.Slug, ver int) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok {
		return false, ErrNotFound
	}
	v, live := p.served()
	return live && v.VerNum == ver, nil
}

// DeleteVersion tombstones a retained version. The served version is refused
// inside the same lock that applies the tombstone.
func (r *MemRepo) DeleteVersion(slug domain.Slug, generation string, ver int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pastes[slug]
	if !ok || p.row.Generation != generation {
		return ErrNotFound
	}
	if served, live := p.served(); live && served.VerNum == ver {
		return domain.ErrVersionCurrentlyServed
	}
	for i := range p.versions {
		if p.versions[i].VerNum == ver {
			p.versions[i].Deleted = true
			p.rollServed()
			return nil
		}
	}
	return nil
}

// --- KeyGateRepo (Sybil admission) ------------------------------------------

func (r *MemRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int,
	window time.Duration,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.keygate[subnet]
	if rows == nil {
		rows = make(map[string]time.Time)
		r.keygate[subnet] = rows
	}
	// Drop out-of-window rows as they are walked past: nothing reads them
	// again, and the family stays bounded by subnets still connecting.
	for id, at := range rows {
		if now.Sub(at) >= window {
			delete(rows, id)
		}
	}
	if _, known := rows[identity]; known {
		// The first-seen stamp must not move, or a returning key would refresh
		// its own window and hold its slot forever.
		return true, nil
	}
	if len(rows) >= limitPerSubnet {
		return false, domain.ErrTooManyNewKeys
	}
	rows[identity] = now
	return false, nil
}

func (r *MemRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var oldest time.Time
	n := 0
	for _, at := range r.keygate[subnet] {
		if now.Sub(at) >= window {
			continue
		}
		n++
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return n, oldest, nil
}

func (r *MemRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rows := range r.keygate {
		if at, ok := rows[identity]; ok && now.Sub(at) < window {
			n++
		}
	}
	return n, nil
}
