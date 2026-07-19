package main

import "fmt"

// checkUnitCountForMode rejects the one incoherent combination of the two
// mode selectors: a bind address (join a cluster) with unit count 0
// (single-backend, no sharding).
//
// It is a startup refusal rather than a silent downgrade to single-node. The
// downgrade is the more dangerous outcome despite looking like the forgiving
// one: the node comes up, reports healthy, and serves traffic exactly like a
// clustered peer, so the missing replication is undetectable until the moment
// it was supposed to save you. Single-node deployments are unaffected - with
// no bind address there is no cluster to be silently absent from.
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
