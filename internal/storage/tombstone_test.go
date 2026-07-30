package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zamua/shale/pkg/cluster"
)

// UNTAGGED on purpose: the scan path these guard is behind the slatedb tag,
// which CI does not build, so a tagged pin would not run in CI.

// The predicate's contract, and specifically the distinction that makes it
// safe: STAMPED-and-empty is a tombstone; BARE-and-empty is live legacy data.
//
// Getting this wrong in the lenient direction leaves the phantom-row bug in
// place. Getting it wrong in the strict direction is worse: it silently drops
// migrated slatedb enumeration entries, which the quota scan sums, so an owner
// would be under-charged with no error surfaced anywhere.
func TestIsTombstoneEnvelope(t *testing.T) {
	stamped := cluster.Stamp{TimestampNanos: 1, NodeID: "n1"}
	for _, tc := range []struct {
		name string
		env  cluster.Envelope
		want bool
	}{
		{"stamped + empty payload is a tombstone", cluster.Envelope{Stamp: stamped}, true},
		{"stamped + nil payload is a tombstone", cluster.Envelope{Stamp: stamped, Payload: nil}, true},
		{"stamped + a value is NOT", cluster.Envelope{Stamp: stamped, Payload: []byte(`{}`)}, false},
		// The legacy case. A bare slatedb marker decodes to the zero Stamp and
		// an empty payload; it is an owner's live enumeration entry.
		{"BARE + empty is NOT a tombstone (legacy marker)", cluster.Envelope{Payload: []byte{}}, false},
		{"BARE + nil is NOT a tombstone (legacy marker)", cluster.Envelope{}, false},
		{"bare + a value is NOT", cluster.Envelope{Payload: markerValue}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTombstoneEnvelope(tc.env); got != tc.want {
				t.Fatalf("isTombstoneEnvelope(%+v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// The distinction must survive a real Encode/Decode round trip, not just hold
// for hand-built structs. If Encode stopped recording the stamp, or Decode
// stopped signalling bareness with the zero stamp, the predicate would quietly
// reclassify one case as the other.
func TestIsTombstoneEnvelope_SurvivesRoundTrip(t *testing.T) {
	tomb, err := cluster.Decode(cluster.Encode(cluster.Envelope{
		Stamp: cluster.Stamp{TimestampNanos: 42, NodeID: "n1"},
	}))
	if err != nil {
		t.Fatalf("decode encoded tombstone: %v", err)
	}
	if !isTombstoneEnvelope(tomb) {
		t.Fatalf("an encoded-then-decoded empty-payload envelope must be a tombstone, got %+v", tomb)
	}

	// A bare empty value, exactly as a migrated slatedb deployment stores it:
	// NOT passed through Encode, because that is the whole point.
	bare, err := cluster.Decode([]byte{})
	if err != nil {
		t.Fatalf("decode bare empty: %v", err)
	}
	if isTombstoneEnvelope(bare) {
		t.Fatalf("a BARE empty value is a live legacy enumeration marker, not a tombstone; "+
			"dropping it under-counts the owner's quota with no error raised: %+v", bare)
	}
}

// THE INVARIANT the tombstone skip rests on: no shale-backend write may store
// an empty value.
//
// isTombstone treats an empty payload as a deleted key and the scan drops it.
// That is only sound because shale's Put REJECTS empty values, so no live value
// can ever be empty - which is precisely why markerValue exists as a one-byte
// stand-in for SlateRepo's []byte{}. If someone later adds an empty-value Put
// on a shale path (reasonably, copying the SlateRepo marker idiom, which DOES
// use []byte{} and has its own separate scanPrefix), that family's rows would
// become invisible to every scan: silently dropped as tombstones, with no
// error anywhere. Data would simply stop being enumerated.
//
// That failure is invisible by construction, so it needs a structural guard
// rather than a behavioural one. The SlateRepo files are deliberately NOT
// checked - their empty markers are correct, they do not share this scan path.
func TestNoShaleWriteStoresAnEmptyValue(t *testing.T) {
	files, err := filepath.Glob("shale_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// Without this the guard silently checks nothing if the files are ever
	// renamed - the same shape of vacuous pass it exists to prevent.
	if len(files) == 0 {
		t.Fatal("found no shale_*.go files to check; re-point this guard rather than leaving it green")
	}

	var checkedPuts int
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Put" && sel.Sel.Name != "Set") {
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			checkedPuts++
			last := call.Args[len(call.Args)-1]
			// []byte{} / []byte(nil) written literally at the call site.
			lit, ok := last.(*ast.CompositeLit)
			if ok && len(lit.Elts) == 0 && isByteSlice(lit.Type) {
				t.Errorf("%s: %s writes an EMPTY value. On the shale backend an empty payload is "+
					"indistinguishable from a delete: shale turns a Delete into an empty-payload tombstone "+
					"Put, and isTombstone/scanPrefixOnce therefore DROP empty payloads from every scan. This "+
					"row family would silently stop being enumerated, with no error raised anywhere. Use "+
					"markerValue (a one-byte '1') instead - that is exactly why it exists.",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checkedPuts == 0 {
		t.Fatal("inspected no Put/Set call sites across shale_*.go, so this guard checked NOTHING")
	}
}

func isByteSlice(e ast.Expr) bool {
	arr, ok := e.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	id, ok := arr.Elt.(*ast.Ident)
	return ok && id.Name == "byte"
}
