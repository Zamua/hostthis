//go:build slatedb

package storage

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestContentRef_WireCompat pins the JSON contract for the served-content
// descriptor: kind/content_sha/blob_id/size must stay at the TOP LEVEL of
// pasteRow / versionRow JSON, never nested under a "contentRef" object, so
// records round-trip against the flat schema in both directions. Naming the
// embedded field or giving it a json tag nests it and silently strands every
// stored record.
func TestContentRef_WireCompat(t *testing.T) {
	// Anonymous embed promotes the fields, so marshaling is flat.
	pr := pasteRow{
		Identity:      "key:o",
		Status:        "ready",
		contentRef:    contentRef{Kind: "html", ContentSHA: "sha1", BlobID: "blob1", Size: 9},
		PinnedVersion: 2,
	}
	b, err := json.Marshal(pr)
	if err != nil {
		t.Fatalf("marshal pasteRow: %v", err)
	}
	js := string(b)
	for _, k := range []string{`"kind":"html"`, `"content_sha":"sha1"`, `"blob_id":"blob1"`, `"size":9`} {
		if !strings.Contains(js, k) {
			t.Fatalf("pasteRow JSON missing flat key %s:\n%s", k, js)
		}
	}
	if strings.Contains(strings.ToLower(js), `"contentref"`) {
		t.Fatalf("pasteRow JSON nested the descriptor (it must stay flat):\n%s", js)
	}

	// Flat JSON decodes into the embedded descriptor AND keeps the row's own
	// fields.
	oldPaste := `{"identity":"key:o","status":"ready","kind":"markdown","content_sha":"oldsha","blob_id":"oldblob","size":42,"name":"n","pinned_version":3}`
	var gotP pasteRow
	if err := json.Unmarshal([]byte(oldPaste), &gotP); err != nil {
		t.Fatalf("unmarshal legacy pasteRow: %v", err)
	}
	if gotP.Kind != "markdown" || gotP.ContentSHA != "oldsha" || gotP.BlobID != "oldblob" || gotP.Size != 42 {
		t.Fatalf("legacy pasteRow lost descriptor fields: %+v", gotP)
	}
	if gotP.PinnedVersion != 3 || gotP.Identity != "key:o" || gotP.Name != "n" {
		t.Fatalf("legacy pasteRow lost its own fields: %+v", gotP)
	}

	// The same contract binds versionRow.
	oldVer := `{"ver_num":5,"kind":"html","content_sha":"vsha","blob_id":"vblob","size":7,"deleted":false}`
	var gotV versionRow
	if err := json.Unmarshal([]byte(oldVer), &gotV); err != nil {
		t.Fatalf("unmarshal legacy versionRow: %v", err)
	}
	if gotV.VerNum != 5 || gotV.Kind != "html" || gotV.ContentSHA != "vsha" || gotV.BlobID != "vblob" || gotV.Size != 7 {
		t.Fatalf("legacy versionRow lost fields: %+v", gotV)
	}
}
