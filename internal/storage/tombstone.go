// Tombstone semantics for the shale backend, and the marker value that exists
// because of them.
//
// DELIBERATELY NOT behind the `slatedb` build tag even though the scan that
// uses them is. CI runs `go test ./...` with no tags, and the guard that keeps
// this sound - no shale write may store an empty value - must run there or it
// guards nothing. Both symbols are pure, so there is nothing to hide. Same
// reasoning as retry_acquiring.go.

package storage

import "github.com/Zamua/shale/pkg/cluster"

// markerValue is the non-empty placeholder for index families that
// carry no data of their own (the expiry index). shale's Put rejects
// empty values, so a one-byte marker stands in for SlateRepo's []byte{}.
var markerValue = []byte{'1'}

// isTombstoneEnvelope reports whether a DECODED scan value is a delete marker
// rather than a row.
//
// At R>1 a delete is not a removal: shale turns it into an empty-payload
// tombstone Put, so the key keeps a stamped envelope whose payload is empty.
// cluster.Get already resolves that to not-found, but a raw ScanPrefix hands
// the stored bytes back, so a scan consumer sees the tombstone as an item.
// That matters because an empty value does not read as "absent" to a consumer,
// it reads as CORRUPT: json.Unmarshal on empty input fails with "unexpected end
// of JSON input". Before this existed, every deleted paste became a permanent
// phantom corrupt row that no retry could repair, re-found and re-processed on
// every reconcile pass for as long as the tombstone lived. Deletes are ordinary
// traffic, so the phantom set only ever grew.
//
// EMPTINESS ALONE IS NOT ENOUGH, and this is the subtle part. Two different
// things arrive as an empty payload:
//
//   - a TOMBSTONE: an envelope (non-zero commit Stamp) with no payload. A
//     deleted key. Must be skipped.
//   - a LEGACY BARE MARKER: a genuinely empty stored value with no envelope at
//     all. slatedb stored the enumeration index as bare empty markers, and the
//     migration to shale is in-place, so the shale scans still encounter those
//     raw empty bytes even though no shale write path can produce them (shale's
//     Put rejects empty values - which is exactly why markerValue exists). This
//     is LIVE DATA: it is an owner's enumeration entry, and the quota scan sums
//     those entries. Skipping it would UNDER-COUNT the quota, which the
//     decode-tolerance ranking invariant forbids outright.
//
// cluster.Decode distinguishes them for us: a bare value passes through with
// the ZERO Stamp, while any real envelope carries the commit stamp it was
// written under. So the test is "stamped AND empty", never "empty".
//
// Pinned by TestShaleQuotaScanLegacyEmptyPasteEntry (the legacy marker must
// survive) and TestIsTombstoneEnvelope (the tombstone must not).
func isTombstoneEnvelope(env cluster.Envelope) bool {
	if env.Stamp == (cluster.Stamp{}) {
		return false // bare legacy value, not a tombstone
	}
	return len(env.Payload) == 0
}
