// Untagged on purpose: the scan using these is behind the slatedb tag, but CI
// builds no tags, and the guard that keeps them sound must run there.

package storage

import "github.com/Zamua/shale/pkg/cluster"

// markerValue stands in for an empty value in index families that carry no data
// of their own. shale's Put rejects empty values.
var markerValue = []byte{'1'}

// isTombstoneEnvelope reports whether a decoded scan value is a deleted key.
//
// At R>1 a delete is an empty-payload write, and a raw scan returns it as an
// item. An empty value decodes as CORRUPT rather than absent, so an unskipped
// tombstone becomes a permanent unrepairable row.
//
// Emptiness alone is not the test. A bare empty value with no envelope is a
// migrated enumeration entry, which is live data the quota scan sums; skipping
// it under-counts a quota. Decode reports bare values with the zero Stamp, so
// the test is stamped AND empty.
func isTombstoneEnvelope(env cluster.Envelope) bool {
	if env.Stamp == (cluster.Stamp{}) {
		return false
	}
	return len(env.Payload) == 0
}
