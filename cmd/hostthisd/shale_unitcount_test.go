package main

import (
	"strings"
	"testing"
)

// A bind address means "join a cluster". A unit count of 0 means
// single-backend. Asking for both is incoherent, and the failure it used to
// produce was the dangerous kind: the node booted, served reads and writes,
// and looked exactly like a clustered peer while providing none of the
// replication it was deployed for. Refusing the boot makes that loud.
func TestRequireUnitCountWhenClustering(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unitCount int
		bindAddr  string
		wantErr   bool
	}{
		{"clustering without sharding is refused", 0, "0.0.0.0:7946", true},
		{"clustering with sharding is fine", 16, "0.0.0.0:7946", false},
		{"single-node without sharding is fine", 0, "", false},
		{"single-node with sharding is fine", 16, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUnitCountForMode(tc.unitCount, tc.bindAddr)
			if tc.wantErr && err == nil {
				t.Fatalf("want a startup refusal, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			// The message must name the env var an operator would have to set.
			// A refusal that does not say which knob to turn is only marginally
			// better than the silent downgrade it replaced.
			if err != nil && !strings.Contains(err.Error(), "HOSTTHIS_SHALE_UNIT_COUNT") {
				t.Fatalf("error must name the env var to set; got %q", err)
			}
		})
	}
}
