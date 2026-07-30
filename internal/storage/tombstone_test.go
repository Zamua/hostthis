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

// Untagged: the scan path is behind the slatedb tag, but CI builds no tags.

// Stamped-and-empty is a tombstone; bare-and-empty is live migrated data.
// Too lenient leaves deleted rows visible as corrupt; too strict silently drops
// enumeration entries the quota scan sums.
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

// The distinction must survive a real round trip: if Decode stopped signalling
// bareness with the zero stamp, the predicate would reclassify one as the other.
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

// The invariant the tombstone skip rests on: no shale write may store an empty
// value.
//
// Sound only because shale's Put rejects empty values, so no live value can be
// empty. An empty-value Put added here would make that family invisible to
// every scan, silently and with no error. SlateRepo is not checked: its empty
// markers are correct and it has its own scan path.
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
