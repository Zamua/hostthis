package domain

import (
	"encoding/json"
	"os"
	"testing"
)

// The asset flag of every known extension equals the table the Worker's read
// rules load from testdata/read-vectors.json.
func TestFileTypesMatchReadVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/read-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Extensions map[string]bool `json:"extensions"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Extensions) != len(fileTypes) {
		t.Fatalf("vectors list %d extensions, fileTypes %d", len(f.Extensions), len(fileTypes))
	}
	for ext, ft := range fileTypes {
		if asset, ok := f.Extensions[ext]; !ok || asset != ft.asset {
			t.Errorf("%s: vectors asset=%v listed=%v, fileTypes asset=%v", ext, asset, ok, ft.asset)
		}
	}
}
