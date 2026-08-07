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
		// A bare slatedb marker decodes to the zero Stamp and an empty
		// payload, and is an owner's live enumeration entry.
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

	// A bare empty value as a migrated slatedb deployment stores it: NOT passed
	// through Encode, which is the whole point.
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
// value. An empty-value Put would make that row family invisible to every scan,
// silently and with no error raised.
func TestNoShaleWriteStoresAnEmptyValue(t *testing.T) {
	files, err := filepath.Glob("shale_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// Without this, renaming the files leaves the guard checking nothing:
	// the same vacuous pass it exists to prevent.
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
			if isEmptyByteValueExpr(last) {
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
	t.Logf("inspected %d Put/Set call sites across %d shale_*.go files", checkedPuts, len(files))
}

// isEmptyByteValueExpr reports whether e is an empty byte value written at the
// call site, in ANY of its syntactic forms.
//
// Deny-by-default over the whole candidate set is the point: a matcher keyed on
// one node type recognises []byte{} and silently admits the equally-empty
// tx.Put(k, nil), while still counting that site as inspected - the guard reads
// green precisely where it is blind.
func isEmptyByteValueExpr(e ast.Expr) bool {
	switch v := unparen(e).(type) {
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.BasicLit:
		return v.Kind == token.STRING && len(v.Value) == 2 // "" or ``
	case *ast.CompositeLit:
		return len(v.Elts) == 0 && isByteSlice(v.Type)
	case *ast.CallExpr:
		// A []byte(x) conversion is empty exactly when x is.
		if len(v.Args) == 1 && isByteSlice(v.Fun) {
			return isEmptyByteValueExpr(v.Args[0])
		}
	}
	return false
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

func isByteSlice(e ast.Expr) bool {
	arr, ok := unparen(e).(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	id, ok := arr.Elt.(*ast.Ident)
	return ok && id.Name == "byte"
}

// The empty-value matcher itself, over every form a call site can spell. Without
// this the guard above is only ever exercised against already-correct code, so
// it can report nothing about the forms it fails to recognise.
func TestIsEmptyByteValueExpr(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{`[]byte{}`, true},
		{`nil`, true},
		{`[]byte(nil)`, true},
		{`([]byte)(nil)`, true},
		{`[]byte("")`, true},
		{"[]byte(``)", true},
		{`([]byte{})`, true},
		{`markerValue`, false},
		{`[]byte("1")`, false},
		{`[]byte{'1'}`, false},
		{`row`, false},
		{`buf.Bytes()`, false},
		{`json.Marshal(row)`, false},
		{`[4]byte{}`, false},
	} {
		t.Run(tc.src, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.src)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.src, err)
			}
			if got := isEmptyByteValueExpr(e); got != tc.want {
				t.Fatalf("isEmptyByteValueExpr(%s) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}
