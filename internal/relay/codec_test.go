package relay

import (
	"encoding/json"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestEncodeSnapshot_MatchesHTTPScanShape(t *testing.T) {
	kv := domain.NewRoomKV()
	kv.Put("name", []byte("alice"))    // opaque -> JSON string
	kv.Put("votes", []byte(`{"a":3}`)) // JSON -> embedded raw JSON
	kv.Put("count", []byte("7"))       // JSON number
	f := encodeSnapshot(kv)
	if f.Binary {
		t.Fatal("snapshot frame should be a text frame")
	}
	var env struct {
		Type  string                     `json:"type"`
		State map[string]json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(f.Data, &env); err != nil {
		t.Fatalf("snapshot did not decode: %v (%q)", err, f.Data)
	}
	if env.Type != TypeSnapshot {
		t.Fatalf("type = %q, want %q", env.Type, TypeSnapshot)
	}
	// Byte-identical to the HTTP scan encoding: JSON values embed raw, an
	// opaque value is a JSON string of the verbatim bytes.
	if string(env.State["votes"]) != `{"a":3}` {
		t.Fatalf("votes = %s, want raw JSON object", env.State["votes"])
	}
	if string(env.State["name"]) != `"alice"` {
		t.Fatalf("name = %s, want JSON string", env.State["name"])
	}
	if string(env.State["count"]) != "7" {
		t.Fatalf("count = %s, want JSON number", env.State["count"])
	}
}

func TestEncodePutAndDelete(t *testing.T) {
	put := EncodePut("k", []byte(`{"x":1}`))
	var penv struct {
		Type  string          `json:"type"`
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(put.Data, &penv); err != nil {
		t.Fatalf("put decode: %v", err)
	}
	if penv.Type != TypePut || penv.Key != "k" || string(penv.Value) != `{"x":1}` {
		t.Fatalf("put env = %+v, want type=put key=k value={\"x\":1}", penv)
	}

	del := EncodeDelete("k")
	var denv struct {
		Type  string          `json:"type"`
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(del.Data, &denv); err != nil {
		t.Fatalf("delete decode: %v", err)
	}
	if denv.Type != TypeDelete || denv.Key != "k" {
		t.Fatalf("delete env = %+v, want type=delete key=k", denv)
	}
	if denv.Value != nil {
		t.Fatalf("delete env carried a value %q, want omitted", denv.Value)
	}
}
