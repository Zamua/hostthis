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
	// TypeReconnect is the drain hint (SPEC "Drain hint:
	// reconnect-before-shutdown"): broadcast once to every local connection
	// the moment the process receives its termination signal, BEFORE the
	// final close, so a client acting on it re-homes (with small random
	// jitter) while its old socket still works. It carries NO seq - it is
	// not a room mutation - and it is an optimization, never load-bearing: a
	// client that ignores it is closed at actual shutdown and heals through
	// the normal reconnect + snapshot + splice path.
	TypeReconnect = "reconnect"
)

// snapshotEnvelope is the late-join control frame. Its State field is the
// SAME key -> value JSON object GET /api/rooms/<uuid> returns, so the same
// client code that loads state on a cold HTTP start consumes the snapshot
// unchanged - only the envelope's "type" tells the client this arrived
// over the relay rather than over HTTP. Seq is the EXACT per-room
// sequence the state reflects: the client initializes lastSeq = Seq and
// discards any durable frame with seq <= lastSeq (the splice contract,
// see SPEC "The client splice contract").
type snapshotEnvelope struct {
	Type  string                     `json:"type"`
	Seq   uint64                     `json:"seq"`
	State map[string]json.RawMessage `json:"state"`
}

// durableEnvelope is the live mirror of a committed durable mutation: a
// PUT (Value set) or a DELETE (Value omitted, Type == TypeDelete). Key is
// the room key that changed. Seq is the mutation's per-room sequence,
// assigned durably at commit - dense (+1 per committed mutation), so a
// subscriber orders by it, de-duplicates by it, and DETECTS a lost frame
// by the hole it leaves. The client applies the frame as
// last-writer-wins on that key, identical to how it applied the key in
// the snapshot, after the seq discard/splice rule admits it.
type durableEnvelope struct {
	Type  string          `json:"type"`
	Seq   uint64          `json:"seq"`
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
	env := snapshotEnvelope{Type: TypeSnapshot, Seq: kv.Seq, State: state}
	data, err := json.Marshal(env)
	if err != nil {
		// Unreachable: every value is valid raw JSON (jsonValue guarantees
		// it). Fall back to an empty snapshot rather than panic.
		data = []byte(`{"type":"snapshot","seq":0,"state":{}}`)
	}
	return Frame{Binary: false, Data: data}
}

// EncodePut builds the live-mirror frame for a durable PUT of (key, val)
// whose commit assigned the per-room sequence seq. The HTTP PUT handler's
// commit closure (passed to Relay.CommitAndMirror) builds this frame AFTER
// the durable write returns the assigned seq, so the frame carries the
// exact position the write landed at in the room's order - the seq every
// subscriber (local or on a peer pod) orders, de-duplicates, and
// gap-detects by.
func EncodePut(seq uint64, key string, val []byte) Frame {
	env := durableEnvelope{Type: TypePut, Seq: seq, Key: key, Value: jsonValue(val)}
	data, err := json.Marshal(env)
	if err != nil {
		return Frame{}
	}
	return Frame{Binary: false, Data: data}
}

// EncodeDelete builds the live-mirror frame for a committed durable DELETE
// of key whose commit assigned the per-room sequence seq.
func EncodeDelete(seq uint64, key string) Frame {
	env := durableEnvelope{Type: TypeDelete, Seq: seq, Key: key}
	data, err := json.Marshal(env)
	if err != nil {
		return Frame{}
	}
	return Frame{Binary: false, Data: data}
}

// encodeReconnect builds the drain-hint control frame. It is a fixed
// envelope (no seq, no payload); see TypeReconnect.
func encodeReconnect() Frame {
	return Frame{Binary: false, Data: []byte(`{"type":"reconnect"}`)}
}

// MaxDurableFrameBytes bounds the largest frame EncodePut can produce for
// a value of at most maxValueBytes. It is the size the peer receiver's
// defense-in-depth cap must admit: jsonValue encodes a non-JSON value as
// a JSON string, and worst-case escaping (\u00XX per control byte)
// inflates it up to 6x, plus the quotes and the envelope (type, seq, key).
// The client-socket cap (Limits.MaxMessageBytes) deliberately does NOT
// apply here - it bounds ephemeral frames from untrusted client sockets,
// while durable mirrors originate from the HTTP PUT path whose value cap
// is several times larger.
func MaxDurableFrameBytes(maxValueBytes int) int64 {
	const envelopeHeadroom = 4 << 10 // type + seq + escaped key + JSON syntax
	return int64(6*maxValueBytes) + envelopeHeadroom
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
