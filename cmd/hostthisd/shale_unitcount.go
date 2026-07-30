package main

import "fmt"

// checkUnitCountForMode rejects the incoherent combination of a bind address
// (join a cluster) with unit count 0 (single-backend, no sharding).
//
// Refusing at startup beats downgrading to single-node: a downgraded node comes
// up, reports healthy, and serves exactly like a clustered peer, so the missing
// replication stays undetectable until it was supposed to save you.
func checkUnitCountForMode(unitCount int, bindAddr string) error {
	if bindAddr != "" && unitCount <= 0 {
		return fmt.Errorf(
			"HOSTTHIS_SHALE_UNIT_COUNT must be > 0 when HOSTTHIS_SHALE_BIND_ADDR is set "+
				"(got %d with bind address %q): a clustered node cannot run single-backend. "+
				"Set a power of two (e.g. 16) to shard, or unset HOSTTHIS_SHALE_BIND_ADDR to run single-node",
			unitCount, bindAddr)
	}
	return nil
}
