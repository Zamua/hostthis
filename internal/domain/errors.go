package domain

import "errors"

// The persistence-outcome sentinel vocabulary. These are the business
// outcomes every storage backend must agree on and every caller matches
// with errors.Is (see docs/SPEC.md "The storage contract and its
// conformance suite"): the domain owns the vocabulary, the backends
// return it, the services translate it.
//
// The storage package re-exports each sentinel under its historical
// storage.Err... name (an alias of the SAME error value), so errors.Is
// identity holds whichever name a caller matches against.
//
// The message text is stable contract: it keeps the historical
// "storage:" prefix VERBATIM because sentinel messages reach
// user-facing output (SSH stderr, HTTP error bodies). Do not edit the
// strings.
var (
	// ErrNotFound is returned by any repo when a lookup misses.
	ErrNotFound = errors.New("storage: not found")

	// ErrSlugTaken is returned by an insert when the chosen slug
	// already exists (as a paste OR a site), and by room creation when
	// the minted (app, id) pair collides. The caller retries with a
	// fresh slug/id.
	ErrSlugTaken = errors.New("storage: slug already taken")

	// ErrOverUserQuota is returned when accepting a write would push an
	// identity's active bytes past its per-user cap.
	ErrOverUserQuota = errors.New("storage: would exceed user quota")

	// ErrServiceFull is returned when the durable total-bytes ceiling
	// is hit: the object store rejects a blob Put because the bucket is
	// at its configured hard quota (see docs/SPEC.md "Limits -> Durable
	// total-bytes ceiling: an object-store quota").
	ErrServiceFull = errors.New("storage: service is at capacity")

	// ErrRoomDataFull is returned when accepting a write would push a
	// single room past its per-room byte or key-count cap. The prior
	// value is left intact. The service layer maps it to a 413.
	ErrRoomDataFull = errors.New("storage: room is at its data cap")

	// ErrAppRoomsFull is returned when accepting a room creation or a
	// write would push an APP's aggregate room bytes past the per-app
	// cap. The service layer maps it to a 507.
	ErrAppRoomsFull = errors.New("storage: app room storage is at capacity")

	// ErrTooManyNewKeys is returned by AdmitNewKey when the subnet has
	// hit its fresh-key quota for the window (the Sybil rate limit).
	ErrTooManyNewKeys = errors.New("storage: too many new keys from this network")

	// ErrCrossShardDeploy is the domain translation of a sharded
	// backend's cross-shard-commit guard rejecting a site deploy's
	// authoritative write (a staged file's pointer routed to a
	// different shard than the manifest - a routing regression the
	// slug pre-claim is designed to prevent). The shale storage layer
	// translates the backend sentinel into this one at the boundary,
	// keeping the original error in the wrap chain, so the deploy
	// service classifies it without importing any backend package.
	ErrCrossShardDeploy = errors.New("storage: cross-shard deploy commit rejected")
)
