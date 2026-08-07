package durable

import (
	"context"
	"slices"
	"sort"
	"sync"
)

// MemLog is an in-memory Log for tests and for backends that have no dual write
// to protect. It is safe for concurrent use, which matters because the
// concurrency tests drive two resolvers at once through it.
type MemLog struct {
	mu sync.Mutex
	in map[Scope]map[ID]Intent

	// FailBegin, when set, makes Begin return it. Crash-boundary tests use this
	// to stop an operation at T0 without killing the process.
	FailBegin error
}

func NewMemLog() *MemLog { return &MemLog{in: map[Scope]map[ID]Intent{}} }

func (m *MemLog) Begin(_ context.Context, in Intent) error {
	if m.FailBegin != nil {
		return m.FailBegin
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.in[in.Scope] == nil {
		m.in[in.Scope] = map[ID]Intent{}
	}
	m.in[in.Scope][in.ID] = in
	return nil
}

func (m *MemLog) Advance(_ context.Context, id ID, scope Scope, step StepName) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.in[scope][id]
	if !ok {
		return nil // already resolved; advancing it is not an error
	}
	if !cur.HasReached(step) {
		cur.Reached = append(slices.Clone(cur.Reached), step)
		m.in[scope][id] = cur
	}
	return nil
}

func (m *MemLog) Complete(_ context.Context, id ID, scope Scope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.in[scope], id)
	return nil
}

func (m *MemLog) Outstanding(_ context.Context, scope Scope) ([]Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Intent, 0, len(m.in[scope]))
	for _, v := range m.in[scope] {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}
