// The metadata plane: the worker adapters at HOSTTHIS_WORKER_ENDPOINT, for
// example http://worker:8787 (docs/SPEC.md "Metadata storage backends").

package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/worker"
)

// metadataBundle is everything the rest of the binary needs from the metadata
// plane.
type metadataBundle struct {
	Repo    metadataRepo
	KeyGate service.KeyGateRepo
	// Denylist is the operator's banned keys and networks.
	Denylist service.DenylistSource
	Sites    siteStore
	// AppRooms lists and removes an app's rooms for its owner.
	AppRooms *worker.AppRoomsRepo
	// Solid forwards the apex /_solid/ paths to the Worker: the apex is not
	// behind Cloudflare (SSH answers on it), so no Worker route reaches them.
	Solid http.Handler
}

// metadataRepo is the union of every service-layer / ssh-layer interface the
// metadata plane has to satisfy: a wiring-layer concern.
type metadataRepo interface {
	service.PasteRepo
	service.PodLinkRepo
	service.PasteAdmin
	hostssh.PasteReader
}

// siteStore is the union of every site-side interface the service / ssh
// layers consume: the deploy view (service.SiteRepo) and the by-slug read
// `url` and `qr` use (hostssh.SiteReader).
type siteStore interface {
	service.SiteRepo
	hostssh.SiteReader
}

// buildMetadata wires the worker adapters.
func buildMetadata(apex string, logger *log.Logger) (*metadataBundle, error) {
	base := envOr("HOSTTHIS_WORKER_ENDPOINT", "")
	if base == "" {
		return nil, errors.New("HOSTTHIS_WORKER_ENDPOINT is required")
	}
	rt, err := workerTransport(base, http.DefaultTransport)
	if err != nil {
		return nil, err
	}
	// One client across every adapter, so connections to the fleet are pooled
	// rather than each surface opening its own.
	client := &http.Client{Timeout: 30 * time.Second, Transport: rt}
	repo := worker.NewPasteRepo(base, client)
	repo.Logger = logger
	solid, err := worker.NewSolidProxy(base, apex, rt)
	if err != nil {
		return nil, err
	}

	logger.Printf("metadata: worker at %s", base)
	return &metadataBundle{
		Repo:     repo,
		KeyGate:  worker.NewKeyGateRepo(base, client),
		Denylist: worker.NewDenylistRepo(base, client),
		Sites:    storage.NewSites(repo),
		AppRooms: worker.NewAppRoomsRepo(base, client),
		Solid:    solid,
	}, nil
}

// workerTransport is rt sending HOSTTHIS_WORKER_TOKEN, when set, to the Worker
// at base. Every client that calls the Worker is built over it.
func workerTransport(base string, rt http.RoundTripper) (http.RoundTripper, error) {
	authed, err := worker.Authorize(rt, base, envOr("HOSTTHIS_WORKER_TOKEN", ""))
	if err != nil {
		return nil, fmt.Errorf("HOSTTHIS_WORKER_TOKEN: %w", err)
	}
	return authed, nil
}
