// metadata_local_slate.go - stub for the local branch when the binary IS built
// with -tags slatedb. That build's storage engine is the object store, so
// there is no local one to open; the real impl is in metadata_local.go.

//go:build slatedb

package main

import (
	"fmt"
	"log"
)

func buildMetadataLocal(_ string, _ *log.Logger) (*metadataBundle, error) {
	return nil, fmt.Errorf(
		"HOSTTHIS_METADATA_BACKEND=local has no engine in a -tags slatedb build; " +
			"set HOSTTHIS_METADATA_BACKEND=shale, or rebuild without the tag")
}
