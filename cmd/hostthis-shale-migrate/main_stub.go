// Stub main for builds WITHOUT the slatedb build tag. The real tool wraps
// the shale slate backend, which requires cgo + libslatedb_uniffi, so it
// only compiles + runs under -tags slatedb. This stub keeps the package
// buildable under the default tag set and fails loudly if someone runs it.

//go:build !slatedb

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"hostthis-shale-migrate requires the 'slatedb' build tag (cgo + libslatedb_uniffi). "+
			"Rebuild with: go build -tags slatedb ./cmd/hostthis-shale-migrate")
	os.Exit(1)
}
