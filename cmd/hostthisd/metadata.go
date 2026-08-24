// metadata.go - pluggable metadata backend selector.
//
// HOSTTHIS_METADATA_BACKEND picks one of:
//
//	local    shale on the local storage engine, the default, no build tag required
//	shale    ShaleRepo, requires -tags slatedb
//
// See docs/SPEC.md "Metadata storage backends".

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
)

// metadataBundle is everything the rest of the binary needs from a metadata
// backend.
type metadataBundle struct {
	Repo    metadataRepo
	KeyGate service.KeyGateRepo
	// Sites is the static-site repo. nil disables static-site archive uploads
	// for that backend, and is nil-safe throughout. Held as the siteStore
	// interface so any backend's impl can be assigned.
	Sites siteStore
	// Rooms is the room-KV repo (the no-auth app-persistence tier). nil
	// disables the /api/rooms surface for that backend, and is nil-safe
	// throughout. Held as the roomStore interface so any backend's impl can be
	// assigned.
	Rooms roomStore
	// BlobUnit is an OPTIONAL backend-supplied blob seam. The shale backend
	// with a blob store configured supplies the transactional shaleblob.Unit,
	// which co-commits the blob pointer with the metadata. nil falls back to
	// the standalone detached-store blob unit, keeping the blob plane
	// decoupled from the metadata plane. See docs/SPEC.md "Shale-collocated
	// blobs".
	BlobUnit service.BlobUnit
	// IntentSweeper is an OPTIONAL backend-supplied boot sweep that settles
	// durable intents left by a process death mid-write. Supplied only by a
	// backend whose writes span shards; a single-transaction backend has no
	// half-finished state to settle. Run ONCE after the server is listening -
	// never before, because settling an intent reads a shard that may not be
	// mounted anywhere during a cold start (docs/SPEC.md "Durable intent").
	IntentSweeper interface {
		SweepIntents(ctx context.Context, now time.Time) (int, error)
		// SweepStagedBytes reclaims the objects an abandoned upload staged and
		// never bound. Separate from the intent sweep because it settles a
		// different thing: an intent decides half-written METADATA, while
		// staged bytes are left by appends, redeploys and by uploads that died
		// before any intent existed (docs/SPEC.md "Staged blob bytes").
		SweepStagedBytes(ctx context.Context, now time.Time) (int, error)
	}
	// RoomRelay is the real-time layer: the cell proxy, since the room cell is
	// the broadcast point. nil disables the /ws surface.
	RoomRelay httpapi.RoomRelay
	// Readiness is the OPTIONAL readiness predicate behind /readyz
	// (docs/SPEC.md "Readiness vs liveness"). The shale backend supplies its
	// mount-floor prober; a backend with no mount concept leaves it nil, which
	// the http server reads as always-ready.
	Readiness httpapi.ReadinessProber
	Close     func() error
}

// metadataRepo is the union of every service-layer / http-layer interface the
// metadata backend has to satisfy. Defined here rather than in
// internal/service because it is a wiring-layer concern.
type metadataRepo interface {
	service.PasteRepo
	service.PasteAdmin
	httpapi.PasteReader
}

// siteStore is the union of every site-side interface the service / http
// layers consume: the deploy view (service.SiteRepo) and the read view
// (httpapi.SiteReader).
type siteStore interface {
	service.SiteRepo
	httpapi.SiteReader
}

// roomStore is the room write/read view the service layer consumes.
type roomStore interface {
	service.RoomRepo
}

// buildMetadata reads HOSTTHIS_METADATA_BACKEND and returns the configured
// bundle, defaulting to the local engine. Each branch errors with a clear
// message when this build cannot serve it: `local` needs a build WITHOUT
// -tags slatedb, `shale` needs one WITH it.
func buildMetadata(dataDir string, logger *log.Logger) (*metadataBundle, error) {
	// Two backends since the shale adapter's removal: celld is the production
	// metadata plane, and memory is the zero-dependency dev/test engine - the
	// SAME MemRepo the conformance suite runs on every `go test ./...`, so what
	// `make run` exercises is contract-identical to what production runs.
	backend := strings.ToLower(envOr("HOSTTHIS_METADATA_BACKEND", "memory"))
	switch backend {
	case "celld":
		return buildMetadataCelld(logger)
	case "memory":
		return buildMetadataMemory(dataDir, logger)
	default:
		return nil, fmt.Errorf("unknown HOSTTHIS_METADATA_BACKEND %q (want celld|memory; shale was removed in v2.1)", backend)
	}
}
