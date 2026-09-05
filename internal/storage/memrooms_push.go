package storage

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// The push surface of MemRoomRepo: subscriptions and schedules with the
// contract's validation and caps, a VAPID public key per app, and the test
// rate limit. Nothing is delivered; delivery is a celld feature. The domain
// rules run here as well as in the service so the storage contract, which the
// conformance suite pins against both backends, refuses the same documents
// the cell does.

type memPushSub struct {
	sub   domain.PushSubscription
	added time.Time
}

type memRoomPush struct {
	subs     []memPushSub
	sched    domain.PushSchedule
	lastTest time.Time
}

func (m *MemRoomRepo) PushKey(app domain.Slug) (string, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	if k, ok := m.r.pushKeys[app]; ok {
		return k, nil
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	k := base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	m.r.pushKeys[app] = k
	return k, nil
}

func (m *MemRoomRepo) PutPushSubscription(app domain.Slug, id domain.RoomID, sub domain.PushSubscription,
	now time.Time,
) error {
	if err := domain.ValidatePushSubscription(sub); err != nil {
		return err
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return ErrNotFound
	}
	for i := range rm.push.subs {
		if rm.push.subs[i].sub.Endpoint == sub.Endpoint {
			rm.push.subs[i].sub = sub
			return nil
		}
	}
	if len(rm.push.subs) >= domain.MaxPushSubscriptions {
		return domain.ErrPushFull
	}
	rm.push.subs = append(rm.push.subs, memPushSub{sub: sub, added: now})
	return nil
}

func (m *MemRoomRepo) DeletePushSubscription(app domain.Slug, id domain.RoomID, endpoint string) error {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return ErrNotFound
	}
	for i := range rm.push.subs {
		if rm.push.subs[i].sub.Endpoint == endpoint {
			rm.push.subs = append(rm.push.subs[:i], rm.push.subs[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *MemRoomRepo) ListPushSubscriptions(app domain.Slug, id domain.RoomID) ([]domain.PushSubscriptionSummary, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]domain.PushSubscriptionSummary, 0, len(rm.push.subs))
	for _, s := range rm.push.subs {
		out = append(out, domain.PushSubscriptionSummary{Endpoint: s.sub.Endpoint, Added: s.added})
	}
	return out, nil
}

func (m *MemRoomRepo) PutPushSchedule(app domain.Slug, id domain.RoomID, sched domain.PushSchedule,
	now time.Time,
) error {
	if err := domain.ValidatePushSchedule(sched, now); err != nil {
		return err
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return ErrNotFound
	}
	rm.push.sched = copySchedule(sched)
	return nil
}

func (m *MemRoomRepo) GetPushSchedule(app domain.Slug, id domain.RoomID) (domain.PushSchedule, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return domain.PushSchedule{}, ErrNotFound
	}
	return copySchedule(rm.push.sched), nil
}

// TestPush enforces the per-room interval and delivers nothing.
func (m *MemRoomRepo) TestPush(app domain.Slug, id domain.RoomID, now time.Time) (domain.PushTestResult, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	rm, ok := m.r.rooms[memRoomKey{app: app, id: id}]
	if !ok {
		return domain.PushTestResult{}, ErrNotFound
	}
	if !rm.push.lastTest.IsZero() {
		if wait := domain.PushTestInterval - now.Sub(rm.push.lastTest); wait > 0 {
			return domain.PushTestResult{}, &domain.PushRateLimit{RetryAfter: wait}
		}
	}
	rm.push.lastTest = now
	return domain.PushTestResult{}, nil
}

// copySchedule detaches the caller's slices from the store's.
func copySchedule(s domain.PushSchedule) domain.PushSchedule {
	out := domain.PushSchedule{TZ: s.TZ, Items: make([]domain.PushItem, len(s.Items))}
	for i, it := range s.Items {
		it.Days = append([]int(nil), it.Days...)
		out.Items[i] = it
	}
	return out
}
