package domain_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

type roomWireVector struct {
	Name string `json:"name"`
	In   string `json:"in"`
	Hex  string `json:"hex"`
	Nest *struct {
		Open  string `json:"open"`
		Inner string `json:"inner"`
		Close string `json:"close"`
		Depth int    `json:"depth"`
	} `json:"nest"`
	JSON  bool    `json:"json"`
	Wire  *string `json:"wire"`
	Value *string `json:"value"`
}

func (v roomWireVector) bytes(t *testing.T) []byte {
	switch {
	case v.Hex != "":
		b, err := hex.DecodeString(v.Hex)
		if err != nil {
			t.Fatalf("%s: hex: %v", v.Name, err)
		}
		return b
	case v.Nest != nil:
		n := v.Nest
		return []byte(strings.Repeat(n.Open, n.Depth) + n.Inner + strings.Repeat(n.Close, n.Depth))
	default:
		return []byte(v.In)
	}
}

// The room wire encoding matches testdata/room-wire-vectors.json, which the
// Worker's port runs too.
func TestRoomWireVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/room-wire-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []roomWireVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range file.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			in := v.bytes(t)
			if got := domain.RoomValueIsJSON(in); got != v.JSON {
				t.Fatalf("RoomValueIsJSON = %v, want %v", got, v.JSON)
			}
			wire := domain.RoomWireValue(in)
			if v.JSON {
				// The cell receives the wire as a JSON string, which reads
				// each byte of an invalid sequence as one U+FFFD.
				want := string([]rune(string(in)))
				if v.Wire != nil {
					want = *v.Wire
				}
				if got := string([]rune(string(wire))); got != want {
					t.Fatalf("wire = %q, want %q", got, want)
				}
				return
			}
			var got string
			if err := json.Unmarshal(wire, &got); err != nil {
				t.Fatalf("wire %q is not a JSON string: %v", wire, err)
			}
			want := string([]rune(string(in)))
			if v.Value != nil {
				want = *v.Value
			}
			if got != want {
				t.Fatalf("decoded wire = %q, want %q", got, want)
			}
		})
	}
}
