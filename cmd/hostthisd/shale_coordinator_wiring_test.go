// Pins that openShaleRepoFromEnv actually CALLS the coordinator guards: the
// helper tests alone stay green if a refactor drops the call sites, and the
// refusals fire before any store contact, so this needs no MinIO.

//go:build slatedb

package main

import (
	"io"
	"log"
	"strings"
	"testing"
)

func TestOpenShaleRepoFromEnvWiresCoordinatorGuards(t *testing.T) {
	t.Setenv("HOSTTHIS_METADATA_S3_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("HOSTTHIS_METADATA_S3_BUCKET", "wiring-test")
	t.Setenv("HOSTTHIS_METADATA_S3_ACCESS_KEY", "k")
	t.Setenv("HOSTTHIS_METADATA_S3_SECRET_KEY", "s")
	t.Setenv("HOSTTHIS_SHALE_UNIT_COUNT", "2")
	t.Setenv("HOSTTHIS_SHALE_HOMOGENEOUS", "true")
	t.Setenv("HOSTTHIS_SHALE_COORDINATOR", "cas")
	t.Setenv("HOSTTHIS_SHALE_GRPC_ADDR", "127.0.0.1:0")
	t.Setenv("HOSTTHIS_SHALE_BIND_ADDR", "127.0.0.1:7946")

	logger := log.New(io.Discard, "", 0)
	_, err := openShaleRepoFromEnv(logger, nil)
	if err == nil || !strings.Contains(err.Error(), "HOSTTHIS_SHALE_BIND_ADDR") {
		t.Fatalf("cas + bind addr through the REAL env path must refuse naming the env var, got: %v", err)
	}
}
