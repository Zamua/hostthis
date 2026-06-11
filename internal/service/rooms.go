package service

import (
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// RoomRepo is the persistence interface the Rooms service needs.
// internal/storage.RoomKVRepo satisfies it. Declared here (not in
// storage) so the service layer owns its dependency contract, the same
// way PasteRepo / SiteRepo / SweepRepo / KeyGateRepo are.
type RoomRepo interface {
	// CreateRoom records a new empty room + its creation-accounting row,
	// enforcing the per-app aggregate cap. ErrSlugTaken if (app, id)
	// collides (caller retries); storage.ErrAppRoomsFull past the app cap.
	CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error
	// GetRoom returns the room record or storage.ErrNotFound.
	GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error)
	// GetValue returns one value or storage.ErrNotFound.
	GetValue(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error)
	// ScanRoom returns the whole namespace.
	ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error)
	// PutValue writes one value, enforcing per-room + per-app + service-wide
	// caps and resetting the retention clock. storage.ErrNotFound if the
	// room is gone, storage.ErrRoomDataFull / storage.ErrAppRoomsFull /
	// storage.ErrServiceFull on caps.
	PutValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap, serviceCap int64, now time.Time) error
	// DeleteValue removes one value (idempotent) and resets the clock.
	DeleteValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) error
	// CountRoomCreates returns in-window creation counts (perSubnet, perApp)
	// for the room-creation rate limit.
	CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error)
}

// Rooms is the application service for the no-auth room persistence tier
// (see SPEC.md "Rooms (app persistence)"). It orchestrates room creation
// (with the per-IP + per-app creation rate limit) and the KV verbs
// (get / put / delete / scan, with the per-room cap enforced in the
// repo). It carries no I/O of its own - the repo is the only adapter.
type Rooms struct {
	Repo RoomRepo

	// Tunables, defaulted by NewRooms; main can override from flags/env.
	MaxRoomsPerIP   int           // per source-IP-subnet creation cap per CreateWindow
	MaxRoomsPerApp  int           // per app creation cap per CreateWindow
	CreateWindow    time.Duration // rolling window for the creation limits
	PerAppByteCap   int64         // per-app aggregate byte cap (0 = unlimited)
	ServiceCapBytes int64         // service-wide cap on total active bytes (0 = unlimited)

	Now func() time.Time
}

// NewRooms wires the SPEC defaults: 60 rooms/IP-subnet/hour,
// 300 rooms/app/hour, 64 MiB per-app aggregate.
func NewRooms(repo RoomRepo) *Rooms {
	return &Rooms{
		Repo:           repo,
		MaxRoomsPerIP:  domain.MaxRoomsPerIPPerWindow,
		MaxRoomsPerApp: domain.MaxRoomsPerAppPerWindow,
		CreateWindow:   domain.RoomCreateWindow,
		PerAppByteCap:  domain.MaxAppRoomBytes,
		Now:            time.Now,
	}
}

// Sentinels the HTTP layer maps to status codes. They are service-layer
// errors so the HTTP handler does not import storage's sentinels
// directly for the room-specific cases.
var (
	// ErrRoomNotFound: the room (or key) does not exist. HTTP 404 - the
	// existence-not-leaked shape.
	ErrRoomNotFound = errors.New("service: room not found")
	// ErrRoomCreateRateLimited: the per-IP or per-app creation rate limit
	// is hit. HTTP 429. The Scope field of *RoomRateLimit says which.
	ErrRoomCreateRateLimited = errors.New("service: room creation rate limit reached")
	// ErrRoomDataCap: a write would exceed the per-room byte/key cap.
	// HTTP 413; the prior value is intact.
	ErrRoomDataCap = errors.New("service: room is at its data cap")
	// ErrAppRoomsCap: the per-app aggregate cap is hit. HTTP 507.
	ErrAppRoomsCap = errors.New("service: app room storage is at capacity")
)

// RoomRateLimit enriches ErrRoomCreateRateLimited with the scope that
// tripped (per-IP vs per-app) and the window, so the HTTP layer can set
// a sensible Retry-After. errors.Is(err, ErrRoomCreateRateLimited) holds.
type RoomRateLimit struct {
	Scope  string // "ip" or "app"
	Window time.Duration
}

func (e *RoomRateLimit) Error() string        { return ErrRoomCreateRateLimited.Error() }
func (e *RoomRateLimit) Is(target error) bool { return target == ErrRoomCreateRateLimited }

// Create mints a fresh UUIDv4 and creates an empty room under appSlug,
// after checking the per-IP-subnet AND per-app creation rate limits.
// subnet is the canonical /24 (IPv4) or /48 (IPv6) the request came from
// (the HTTP layer derives it, reusing the same shape the SSH Sybil gate
// uses). Returns the created Room.
//
// On rate-limit returns *RoomRateLimit (also errors.Is ErrRoomCreateRateLimited).
// On per-app aggregate full returns ErrAppRoomsCap.
//
// The rate-limit count is read OUTSIDE the CreateRoom transaction, so the
// creation gate is a SOFT bound: N concurrent creators can each observe
// the same in-window count and all pass before their accounting rows
// commit, slightly overshooting the cap. That is an accepted trade for a
// coarse abuse bound (the hard structural bounds are the per-app aggregate
// byte cap and the service-wide cap, both enforced inside the write tx).
func (s *Rooms) Create(appSlug domain.Slug, subnet string) (domain.Room, error) {
	now := s.now()

	perSubnet, perApp, err := s.Repo.CountRoomCreates(appSlug, subnet, now, s.CreateWindow)
	if err != nil {
		return domain.Room{}, err
	}
	if s.MaxRoomsPerIP > 0 && perSubnet >= s.MaxRoomsPerIP {
		return domain.Room{}, &RoomRateLimit{Scope: "ip", Window: s.CreateWindow}
	}
	if s.MaxRoomsPerApp > 0 && perApp >= s.MaxRoomsPerApp {
		return domain.Room{}, &RoomRateLimit{Scope: "app", Window: s.CreateWindow}
	}

	// Retry on the astronomically-unlikely (app, id) collision.
	const maxRetries = 5
	for range maxRetries {
		room := domain.Room{
			AppSlug:   appSlug,
			ID:        domain.NewRoomID(),
			CreatedAt: now,
			UpdatedAt: now,
			ExpiresAt: now.Add(domain.RoomRetentionWindow),
		}
		err := s.Repo.CreateRoom(room, subnet, s.PerAppByteCap, now)
		switch {
		case err == nil:
			return room, nil
		case errors.Is(err, storage.ErrAppRoomsFull):
			return domain.Room{}, ErrAppRoomsCap
		case isSlugTaken(err):
			continue
		default:
			return domain.Room{}, err
		}
	}
	return domain.Room{}, SlugTakenErr
}

// Get returns one value from a room. It checks the room exists first so a
// missing ROOM and a missing KEY both surface as ErrRoomNotFound (the
// 404 that does not leak room existence), distinct from a value that is
// present. key must already be validated by the caller.
func (s *Rooms) Get(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	if _, err := s.Repo.GetRoom(appSlug, id); err != nil {
		return nil, s.mapNotFound(err)
	}
	val, err := s.Repo.GetValue(appSlug, id, key)
	if err != nil {
		return nil, s.mapNotFound(err)
	}
	return val, nil
}

// Scan returns the room's whole namespace as a domain.RoomKV, after
// verifying the room exists (a scan of a nonexistent room is a 404, not
// an empty 200, so existence is not leaked the other way either).
func (s *Rooms) Scan(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	if _, err := s.Repo.GetRoom(appSlug, id); err != nil {
		return domain.RoomKV{}, s.mapNotFound(err)
	}
	kv, err := s.Repo.ScanRoom(appSlug, id)
	if err != nil {
		return domain.RoomKV{}, err
	}
	return kv, nil
}

// Put writes val under key, enforcing the per-room data cap (in the repo),
// the per-app aggregate, and the service-wide cap. Returns ErrRoomNotFound
// if the room is gone, ErrRoomDataCap (413) on the per-room cap,
// ErrAppRoomsCap (507) on the per-app aggregate OR the service-wide cap
// (both are "no room to store this" - a 507). key must already be
// validated by the caller.
func (s *Rooms) Put(appSlug domain.Slug, id domain.RoomID, key string, val []byte) error {
	now := s.now()
	err := s.Repo.PutValue(appSlug, id, key, val, s.PerAppByteCap, s.ServiceCapBytes, now)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrNotFound):
		return ErrRoomNotFound
	case errors.Is(err, storage.ErrRoomDataFull):
		return ErrRoomDataCap
	case errors.Is(err, storage.ErrAppRoomsFull):
		return ErrAppRoomsCap
	case errors.Is(err, storage.ErrServiceFull):
		// The service-wide cap is the same "no capacity to store this"
		// shape as the per-app aggregate from the writer's view: a 507.
		return ErrAppRoomsCap
	default:
		return err
	}
}

// Delete removes key (idempotent) and resets the room's retention clock.
// Returns ErrRoomNotFound only when the ROOM does not exist; deleting an
// absent key in a real room is a success. key must already be validated.
func (s *Rooms) Delete(appSlug domain.Slug, id domain.RoomID, key string) error {
	now := s.now()
	if err := s.Repo.DeleteValue(appSlug, id, key, now); err != nil {
		return s.mapNotFound(err)
	}
	return nil
}

func (s *Rooms) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// mapNotFound translates the storage not-found sentinel to the service
// one, passing any other error through.
func (s *Rooms) mapNotFound(err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return ErrRoomNotFound
	}
	return err
}
