package storage

import (
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// The room surface of MemRepo. One mutex with the paste surface, so the
// cross-family interactions the conformance suite pins (shared slug namespace,
// app-scoped budgets) are exercised against one store.

type memRoomKey struct {
	app domain.Slug
	id  domain.RoomID
}

type memRoom struct {
	meta  domain.Room
	kv    map[string][]byte
	bytes int
	seq   uint64
	push  memRoomPush
}

type memRoomCreate struct {
	subnet string
	at     time.Time
}

// MemRoomRepo is the service.RoomRepo view of a MemRepo. A separate type only
// so the composition mirrors production wiring, where the room repo is its own
// adapter object.
type MemRoomRepo struct{ r *MemRepo }

func NewMemRoomRepo(r *MemRepo) *MemRoomRepo { return &MemRoomRepo{r: r} }

func (m *MemRoomRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	k := memRoomKey{app: room.AppSlug, id: room.ID}
	if _, exists := m.r.rooms[k]; exists {
		return nil
	}
	if appCap > 0 && m.r.roomBytes[room.AppSlug] >= appCap {
		return ErrAppRoomsFull
	}
	m.r.rooms[k] = &memRoom{meta: room, kv: make(map[string][]byte)}
	m.r.roomLedger[room.AppSlug] = append(m.r.roomLedger[room.AppSlug], memRoomCreate{subnet: subnet, at: now})
	return nil
}

func (m *MemRoomRepo) GetRoom(app domain.Slug, id domain.RoomID) (domain.Room, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return domain.Room{}, ErrNotFound
	}
	return rm.meta, nil
}

func (m *MemRoomRepo) GetValue(app domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return nil, ErrNotFound
	}
	v, ok := rm.kv[key]
	if !ok {
		return nil, ErrNotFound
	}
	// A stored empty value is a real value; the non-nil empty slice keeps it
	// distinguishable from absence for callers comparing against nil.
	if v == nil {
		return []byte{}, nil
	}
	return append([]byte(nil), v...), nil
}

func (m *MemRoomRepo) ScanRoom(app domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return domain.RoomKV{}, ErrNotFound
	}
	out := domain.RoomKV{Values: make(map[string][]byte, len(rm.kv)), Seq: rm.seq}
	for k, v := range rm.kv {
		out.Values[k] = append([]byte(nil), v...)
	}
	return out, nil
}

// PutValue enforces the per-room byte and key caps and the app's aggregate
// budget in one critical section, so every cap is EXACT here: whatever a
// concurrent burst lands, the totals never exceed a cap.
func (m *MemRoomRepo) PutValue(app domain.Slug, id domain.RoomID, key string, val []byte,
	appCap int64, now time.Time,
) (uint64, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return 0, ErrNotFound
	}
	prior := len(rm.kv[key])
	_, had := rm.kv[key]
	next := rm.bytes - prior + len(val)
	if !had && len(rm.kv) >= domain.MaxRoomKeys {
		return 0, ErrRoomDataFull
	}
	if next > domain.MaxRoomBytes {
		return 0, ErrRoomDataFull
	}
	if appCap > 0 {
		total := m.r.roomBytes[app] - int64(rm.bytes) + int64(next)
		if total > appCap {
			return 0, ErrAppRoomsFull
		}
	}
	m.r.roomBytes[app] += int64(next - rm.bytes)
	rm.kv[key] = append([]byte(nil), val...)
	rm.bytes = next
	rm.seq++
	rm.meta.UpdatedAt = now
	return rm.seq, nil
}

// DeleteValue commits and consumes a sequence number even for an absent key: a
// subscriber splicing a live stream onto a snapshot reads a skipped seq as a
// lost frame.
func (m *MemRoomRepo) DeleteValue(app domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return 0, ErrNotFound
	}
	if v, had := rm.kv[key]; had {
		rm.bytes -= len(v)
		m.r.roomBytes[app] -= int64(len(v))
		delete(rm.kv, key)
	}
	rm.seq++
	rm.meta.UpdatedAt = now
	return rm.seq, nil
}

// CountRoomCreates counts the in-window creation-ledger rows, pruning the
// aged-out ones on the way past so the ledger stays bounded with no sweep.
func (m *MemRoomRepo) CountRoomCreates(app domain.Slug, subnet string, now time.Time,
	window time.Duration,
) (int, int, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	ledger := m.r.roomLedger[app]
	live := ledger[:0]
	perSubnet, perApp := 0, 0
	for _, e := range ledger {
		if now.Sub(e.at) >= window {
			continue
		}
		live = append(live, e)
		perApp++
		if e.subnet == subnet {
			perSubnet++
		}
	}
	m.r.roomLedger[app] = live
	return perSubnet, perApp, nil
}
