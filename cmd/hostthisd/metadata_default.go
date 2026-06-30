// metadata_default.go - stub for the slatedb branch when the binary
// is NOT built with -tags slatedb. The real impl is in
// metadata_slatedb.go which has the matching build tag.

//go:build !slatedb

package main

import (
	"fmt"
	"log"

	"github.com/Zamua/hostthis/internal/domain"
)

func buildMetadataSlate(_ domain.Retention, _ *log.Logger) (*metadataBundle, error) {
	return nil, fmt.Errorf(
		"HOSTTHIS_METADATA_BACKEND=slatedb requires a binary built with -tags slatedb; " +
			"rebuild via `go build -tags slatedb` (and ensure libslatedb_uniffi is on the loader path)")
}

func buildMetadataShale(_ domain.Retention, _ *log.Logger) (*metadataBundle, error) {
	return nil, fmt.Errorf(
		"HOSTTHIS_METADATA_BACKEND=shale requires a binary built with -tags slatedb; " +
			"rebuild via `go build -tags slatedb` (and ensure libslatedb_uniffi is on the loader path)")
}
