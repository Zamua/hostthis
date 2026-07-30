package relay

import (
	"encoding/json"

	"github.com/Zamua/hostthis/internal/domain"
)

// The only server-originated frames, which the client distinguishes by their
// "type" field. A relayed PEER frame is never stamped: those stay
// payload-opaque and fan out verbatim.
const (
	// TypeSnapshot is the first frame a joining client receives: the full
	// current room state, so a late joiner is caught up before the live stream
	// begins.
	TypeSnapshot = "snapshot"
	// TypePut is the live mirror of a committed durable PUT. The client applies
	// it exactly as it applied that key in the snapshot (last-writer-wins per
	// key), so a stray re-delivery is idempotent.
	TypePut = "put"
	// TypeDelete is the live mirror of a committed durable DELETE.
	TypeDelete = "delete"
	// TypeReconnect is the drain hint (SPEC "Drain hint:
	// reconnect-before-shutdown"), broadcast to every local connection on the
	// termination signal and BEFORE the final close, so a client acting on it
	// re-homes while its old socket still works. It carries no seq, being no
	// room mutation, and is never load-bearing: a client that ignores it is
	// closed at shutdown and heals through reconnect + snapshot + splice.
	TypeReconnect = "reconnect"
)

// snapshotEnvelope is the late-join control frame. State is the SAME key ->
// value JSON object GET /api/rooms/<uuid> returns, so the client code that
// loads state on a cold HTTP start consumes a snapshot unchanged. Seq is the
// exact per-room sequence that state reflects: the client sets lastSeq = Seq
// and discards any durable frame with seq <= lastSeq (SPEC "The client splice
// contract").
type snapshotEnvelope struct {
	Type  string                     `json:"type"`
	Seq   uint64                     `json:"seq"`
	State map[string]json.RawMessage `json:"state"`
}

// durableEnvelope is the live mirror of a committed durable mutation: a PUT
// (Value set) or a DELETE (Value omitted). Seq is the mutation's per-room
// sequence, assigned durably at commit and DENSE (+1 per committed mutation),
// so a subscriber orders by it, de-duplicates by it, and detects a lost frame
// by the hole it leaves.
type durableEnvelope struct {
	Type  string          `json:"type"`
	Seq   uint64          `json:"seq"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value,omitempty"`
}

// encodeSnapshot builds the late-join snapshot frame from a room KV. Values go
// through domain.RoomWireValue, the same encoder the HTTP scan handler uses, so
// a snapshot and a cold-start GET are byte-for-byte interchangeable.
func encodeSnapshot(kv domain.RoomKV) Frame {
	state := make(map[string]json.RawMessage, kv.KeyCount())
	for k, v := range kv.Values {
		state[k] = domain.RoomWireValue(v)
	}
	env := snapshotEnvelope{Type: TypeSnapshot, Seq: kv.Seq, State: state}
	data, err := json.Marshal(env)
	if err != nil {
		// Unreachable: RoomWireValue guarantees every value is valid raw JSON.
		// An empty snapshot beats a panic.
		data = []byte(`{"type":"snapshot","seq":0,"state":{}}`)
	}
	return Frame{Binary: false, Data: data}
}

// EncodePut builds the live-mirror frame for a durable PUT of (key, val) whose
// commit assigned the per-room sequence seq. The commit closure passed to
// Relay.CommitAndMirror builds the frame only AFTER the durable write returns
// that seq, so it carries the exact position the write landed at in the room's
// order.
func EncodePut(seq uint64, key string, val []byte) Frame {
	env := durableEnvelope{Type: TypePut, Seq: seq, Key: key, Value: domain.RoomWireValue(val)}
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

// encodeReconnect builds the drain-hint control frame: a fixed envelope with no
// seq and no payload. See TypeReconnect.
func encodeReconnect() Frame {
	return Frame{Binary: false, Data: []byte(`{"type":"reconnect"}`)}
}

// MaxDurableFrameBytes bounds the largest frame EncodePut can produce for a
// value of at most maxValueBytes, and is what the peer receiver's cap must
// admit. RoomWireValue encodes a non-JSON value as a JSON string, and
// worst-case escaping (\u00XX per control byte) inflates it up to 6x, plus
// quotes and the envelope.
//
// The client-socket cap (Limits.MaxMessageBytes) deliberately does NOT apply:
// it bounds ephemeral frames from untrusted client sockets, while durable
// mirrors come from the HTTP PUT path, whose value cap is several times larger.
func MaxDurableFrameBytes(maxValueBytes int) int64 {
	const envelopeHeadroom = 4 << 10 // type + seq + escaped key + JSON syntax
	return int64(6*maxValueBytes) + envelopeHeadroom
}
