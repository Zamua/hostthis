package main

import (
	"strings"
	"testing"
)

// The cas coordinator means "join a cluster" and a unit count of 0 means
// single-backend, so asking for both is incoherent. The boot is refused rather
// than downgraded, because a downgraded node serves reads and writes and looks
// exactly like a clustered peer while providing none of the replication it was
// deployed for.
func TestRequireUnitCountWhenClustering(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unitCount int
		coordMode string
		wantErr   bool
	}{
		{"clustering without sharding is refused", 0, "cas", true},
		{"clustering with sharding is fine", 16, "cas", false},
		{"single-node without sharding is fine", 0, "", false},
		{"single-node with sharding is fine", 16, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUnitCountForMode(tc.unitCount, tc.coordMode)
			if tc.wantErr && err == nil {
				t.Fatalf("want a startup refusal, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			// The message must name the env var to set: a refusal that does not
			// say which knob to turn is barely better than a silent downgrade.
			if err != nil && !strings.Contains(err.Error(), "HOSTTHIS_SHALE_UNIT_COUNT") {
				t.Fatalf("error must name the env var to set; got %q", err)
			}
		})
	}
}
