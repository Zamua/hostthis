// ============================================================================
// TEMPORARY phase-4 migration code - DELETE THIS DIRECTORY
// (cmd/hostthis-blob-migrate/) after the prod blob migration completes, along
// with internal/storage/shale_blob_migrate_TEMP.go and the one temp Dockerfile
// build+COPY line. See docs/design/shale-blobs-phase4-migration.md.
// ============================================================================

// Stub main for builds WITHOUT the slatedb build tag. The real tool constructs
// the full ShaleRepo (which wraps slatedb via the shale slate backend) and so
// requires cgo + libslatedb_uniffi; it only compiles + runs under -tags
// slatedb. This stub keeps the package buildable under the default tag set
// (so `go build ./...` / `go vet ./...` stay green) and fails loudly if someone
// runs it. Mirrors cmd/hostthis-shale-migrate's main_stub.go.

//go:build !slatedb

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"hostthis-blob-migrate requires the 'slatedb' build tag (cgo + libslatedb_uniffi). "+
			"Rebuild with: go build -tags slatedb ./cmd/hostthis-blob-migrate")
	os.Exit(1)
}
