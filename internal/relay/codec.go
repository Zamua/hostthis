package relay

import (
	"encoding/json"

	"github.com/Zamua/hostthis/internal/domain"
)

// The relay's ONLY server-originated frames are two control envelopes the
// client distinguishes by their "type" field. hostthis never stamps
// anything onto a relayed PEER frame (those stay payload-opaque, fanned
// out verbatim) - these envelopes carry only server-originated state: the
// late-join snapshot and the live mirror of a durable PUT/DELETE.
const (
	// TypeSnapshot is the first frame a joining client receives: its full
	// current room state, so a late joiner (including a reloaded page) is
	// caught up before the live stream begins.
	TypeSnapshot = "snapshot"
	// TypePut is the live mirror of a committed durable PUT: a single key
	// was set. The client applies it the same way it applied that key in
	// the snapshot (last-writer-wins per key), so a stray re-delivery is
	// idempotent.
	TypePut = "put"
	// TypeDelete is the live mirror of a committed durable DELETE: a single
	// key was removed.
	TypeDelete = "delete"
)

// snapshotEnvelope is the late-join control frame. Its State field is the
// SAME key -> value JSON object GET /api/rooms/<uuid> returns, so the same
// client code that loads state on a cold HTTP start consumes the snapshot
// unchanged - only the envelope's "type" tells the client this arrived
// over the relay rather than over HTTP.
type snapshotEnvelope struct {
	Type  string                     `json:"type"`
	State map[string]json.RawMessage `json:"state"`
}

// durableEnvelope is the live mirror of a committed durable mutation: a
// PUT (Value set) or a DELETE (Value omitted, Type == TypeDelete). Key is
// the room key that changed. The client applies it as last-writer-wins on
// that key, identical to how it applied the key in the snapshot.
type durableEnvelope struct {
	Type  string          `json:"type"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value,omitempty"`
}

// encodeSnapshot builds the late-join snapshot frame from a room KV. The
// State object embeds each value as raw JSON when it parses as JSON, else
// as a JSON string of the verbatim bytes - byte-for-byte the encoding the
// HTTP scan handler uses, so the snapshot and a cold-start GET are
// interchangeable on the client.
func encodeSnapshot(kv domain.RoomKV) Frame {
	state := make(map[string]json.RawMessage, kv.KeyCount())
	for k, v := range kv.Values {
		state[k] = jsonValue(v)
	}
	env := snapshotEnvelope{Type: TypeSnapshot, State: state}
	data, err := json.Marshal(env)
	if err != nil {
		// Unreachable: every value is valid raw JSON (jsonValue guarantees
		// it). Fall back to an empty snapshot rather than panic.
		data = []byte(`{"type":"snapshot","state":{}}`)
	}
	return Frame{Binary: false, Data: data}
}

// EncodePut builds the live-mirror frame for a committed durable PUT of
// (key, val). The HTTP PUT handler calls it after the write commits and
// passes the frame to Relay.MirrorDurable so the room's connected clients
// see the change live, in addition to it landing in the durable KV.
func EncodePut(key string, val []byte) Frame {
	env := durableEnvelope{Type: TypePut, Key: key, Value: jsonValue(val)}
	data, err := json.Marshal(env)
	if err != nil {
		return Frame{}
	}
	return Frame{Binary: false, Data: data}
}

// EncodeDelete builds the live-mirror frame for a committed durable DELETE
// of key.
func EncodeDelete(key string) Frame {
	env := durableEnvelope{Type: TypeDelete, Key: key}
	data, err := json.Marshal(env)
	if err != nil {
		return Frame{}
	}
	return Frame{Binary: false, Data: data}
}

// jsonValue returns v as raw JSON when it already parses as JSON, else as
// a JSON string of the verbatim bytes. This mirrors the HTTP scan
// handler's jsonValue so a relay snapshot/mirror encodes a value
// identically to the HTTP KV surface.
func jsonValue(v []byte) json.RawMessage {
	if len(v) > 0 && json.Valid(v) {
		return json.RawMessage(v)
	}
	encoded, err := json.Marshal(string(v))
	if err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(encoded)
}
