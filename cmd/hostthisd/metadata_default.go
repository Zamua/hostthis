// metadata_default.go - stub for the shale branch when this build cannot serve
// it. The real impl is in metadata_shale.go, which carries the matching build
// tag.

//go:build !slatedb

package main

import (
	"fmt"
	"log"
)

func buildMetadataShale(_ *log.Logger) (*metadataBundle, error) {
	return nil, fmt.Errorf(
		"HOSTTHIS_METADATA_BACKEND=shale requires a binary built with -tags slatedb; " +
			"rebuild via `go build -tags slatedb` (and ensure libslatedb_uniffi is on the loader path)")
}
