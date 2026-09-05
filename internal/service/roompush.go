package service

import (
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// RoomPushRepo is the persistence contract for room push (SPEC.md "Room push
// (scheduled Web Push)"). Every room-scoped method returns domain.ErrNotFound
// for a missing room. storage.MemRoomRepo and celld.RoomRepo satisfy it.
type RoomPushRepo interface {
	// PushKey returns the app's VAPID public key (base64url of the 65-byte
	// uncompressed P-256 point), generating the pair on first use.
	PushKey(app domain.Slug) (string, error)
	// PutPushSubscription adds or refreshes a subscription, deduplicated by
	// endpoint. domain.ErrPushFull when a new endpoint would exceed the cap.
	PutPushSubscription(app domain.Slug, id domain.RoomID, sub domain.PushSubscription, now time.Time) error
	// DeletePushSubscription removes one endpoint; idempotent.
	DeletePushSubscription(app domain.Slug, id domain.RoomID, endpoint string) error
	// ListPushSubscriptions returns endpoints and when each was added.
	ListPushSubscriptions(app domain.Slug, id domain.RoomID) ([]domain.PushSubscriptionSummary, error)
	// PutPushSchedule replaces the schedule; empty items clears it. now is the
	// instant a one-shot item must fall after.
	PutPushSchedule(app domain.Slug, id domain.RoomID, sched domain.PushSchedule, now time.Time) error
	// GetPushSchedule returns the schedule, with empty items when unset.
	GetPushSchedule(app domain.Slug, id domain.RoomID) (domain.PushSchedule, error)
	// TestPush sends a test notification to every subscription inline.
	// *domain.PushRateLimit within domain.PushTestInterval of the last one.
	TestPush(app domain.Slug, id domain.RoomID, now time.Time) (domain.PushTestResult, error)
}

// Sentinels the HTTP layer maps to status codes.
var (
	// ErrPushCap: the room holds MaxPushSubscriptions endpoints. HTTP 413.
	ErrPushCap = errors.New("service: room is at its push subscription cap")
	// ErrPushRateLimited: HTTP 429; *PushRateLimit carries Retry-After.
	ErrPushRateLimited = errors.New("service: push test rate limit reached")
)

// PushRateLimit enriches ErrPushRateLimited with the wait.
type PushRateLimit struct{ RetryAfter time.Duration }

func (e *PushRateLimit) Error() string        { return ErrPushRateLimited.Error() }
func (e *PushRateLimit) Is(target error) bool { return target == ErrPushRateLimited }

// RoomPush is the application service for room push: validation in front of
// the repo, and the repo is its only adapter. Invalid input surfaces as
// domain.ErrPushInvalid (wrapped, with detail).
type RoomPush struct {
	Repo RoomPushRepo
	Now  func() time.Time
}

func NewRoomPush(repo RoomPushRepo) *RoomPush {
	return &RoomPush{Repo: repo, Now: time.Now}
}

// Key returns the app's VAPID public key.
func (s *RoomPush) Key(app domain.Slug) (string, error) {
	return s.Repo.PushKey(app)
}

func (s *RoomPush) PutSubscription(app domain.Slug, id domain.RoomID, sub domain.PushSubscription) error {
	if err := domain.ValidatePushSubscription(sub); err != nil {
		return err
	}
	return s.mapErr(s.Repo.PutPushSubscription(app, id, sub, s.now()))
}

func (s *RoomPush) DeleteSubscription(app domain.Slug, id domain.RoomID, endpoint string) error {
	if endpoint == "" || len(endpoint) > domain.MaxPushEndpointBytes {
		return domain.ErrPushInvalid
	}
	return s.mapErr(s.Repo.DeletePushSubscription(app, id, endpoint))
}

func (s *RoomPush) ListSubscriptions(app domain.Slug, id domain.RoomID) ([]domain.PushSubscriptionSummary, error) {
	subs, err := s.Repo.ListPushSubscriptions(app, id)
	if err != nil {
		return nil, s.mapErr(err)
	}
	if subs == nil {
		subs = []domain.PushSubscriptionSummary{}
	}
	return subs, nil
}

func (s *RoomPush) PutSchedule(app domain.Slug, id domain.RoomID, sched domain.PushSchedule) error {
	now := s.now()
	if err := domain.ValidatePushSchedule(sched, now); err != nil {
		return err
	}
	if sched.Items == nil {
		sched.Items = []domain.PushItem{}
	}
	return s.mapErr(s.Repo.PutPushSchedule(app, id, sched, now))
}

func (s *RoomPush) GetSchedule(app domain.Slug, id domain.RoomID) (domain.PushSchedule, error) {
	sched, err := s.Repo.GetPushSchedule(app, id)
	if err != nil {
		return domain.PushSchedule{}, s.mapErr(err)
	}
	if sched.Items == nil {
		sched.Items = []domain.PushItem{}
	}
	return sched, nil
}

func (s *RoomPush) Test(app domain.Slug, id domain.RoomID) (domain.PushTestResult, error) {
	res, err := s.Repo.TestPush(app, id, s.now())
	if err != nil {
		return domain.PushTestResult{}, s.mapErr(err)
	}
	return res, nil
}

func (s *RoomPush) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *RoomPush) mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return ErrRoomNotFound
	case errors.Is(err, domain.ErrPushFull):
		return ErrPushCap
	case errors.Is(err, domain.ErrPushRateLimited):
		rl := &PushRateLimit{RetryAfter: domain.PushTestInterval}
		if d, ok := errors.AsType[*domain.PushRateLimit](err); ok && d.RetryAfter > 0 {
			rl.RetryAfter = d.RetryAfter
		}
		return rl
	default:
		return err
	}
}
