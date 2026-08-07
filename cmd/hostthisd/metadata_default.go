// metadata_default.go - stubs for the branches this build cannot serve. The
// real impls are in metadata_slatedb.go / metadata_shale.go, which carry the
// matching build tag.

//go:build !slatedb

package main

import (
	"fmt"
	"log"
)

func buildMetadataSlate(_ *log.Logger) (*metadataBundle, error) {
	return nil, fmt.Errorf(
		"HOSTTHIS_METADATA_BACKEND=slatedb requires a binary built with -tags slatedb; " +
			"rebuild via `go build -tags slatedb` (and ensure libslatedb_uniffi is on the loader path)")
}

func buildMetadataShale(_ *log.Logger) (*metadataBundle, error) {
	return nil, fmt.Errorf(
		"HOSTTHIS_METADATA_BACKEND=shale requires a binary built with -tags slatedb; " +
			"rebuild via `go build -tags slatedb` (and ensure libslatedb_uniffi is on the loader path)")
}
