package http

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// RoomPushService is the application-service surface for room push
// (SPEC.md "Room push (scheduled Web Push)"). internal/service.RoomPush
// satisfies it. Optional: when nil the push routes 404.
type RoomPushService interface {
	Key(appSlug domain.Slug) (string, error)
	PutSubscription(appSlug domain.Slug, id domain.RoomID, sub domain.PushSubscription) error
	DeleteSubscription(appSlug domain.Slug, id domain.RoomID, endpoint string) error
	ListSubscriptions(appSlug domain.Slug, id domain.RoomID) ([]domain.PushSubscriptionSummary, error)
	PutSchedule(appSlug domain.Slug, id domain.RoomID, sched domain.PushSchedule) error
	GetSchedule(appSlug domain.Slug, id domain.RoomID) (domain.PushSchedule, error)
	Test(appSlug domain.Slug, id domain.RoomID) (domain.PushTestResult, error)
}

// pushKeyPath serves the app's VAPID public key at the app origin, beside the
// rooms API and carved out of the site path space the same way.
const pushKeyPath = "/api/push/key"

// pushMaxBodyBytes bounds the JSON documents the push routes accept. A
// schedule at every cap is under 32 KiB; a subscription is a few hundred
// bytes.
const pushMaxBodyBytes = 64 << 10

// handlePushKey serves GET /api/push/key for appSlug.
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request, appSlug domain.Slug) {
	if s.RoomPush == nil || !s.appExists(appSlug) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		roomMethodNotAllowed(w, http.MethodGet)
		return
	}
	key, err := s.RoomPush.Key(appSlug)
	if err != nil {
		s.writePushError(w, r, err)
		return
	}
	writePushJSON(w, http.StatusOK, struct {
		Key string `json:"key"`
	}{Key: key})
}

// handleRoomPush serves /api/rooms/<uuid>/push/<sub> with id already parsed.
// sub is the remainder after "push": "/subscriptions", "/schedule", "/test".
// Any other shape is the reserved-key 400 the spec promises for a KV verb
// aimed at "push" or under "push/". The app check comes first so an unknown
// slug 404s for every shape, the same as the rest of the room API.
func (s *Server) handleRoomPush(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID, sub string) {
	if !s.appExists(appSlug) {
		http.NotFound(w, r)
		return
	}
	switch sub {
	case "/subscriptions", "/schedule", "/test":
	default:
		http.Error(w, "reserved key\n", http.StatusBadRequest)
		return
	}
	if s.RoomPush == nil {
		http.NotFound(w, r)
		return
	}
	switch sub {
	case "/subscriptions":
		switch r.Method {
		case http.MethodPut:
			var subscription domain.PushSubscription
			if !decodePushBody(w, r, &subscription) {
				return
			}
			s.pushNoContent(w, r, s.RoomPush.PutSubscription(appSlug, id, subscription))
		case http.MethodDelete:
			var body struct {
				Endpoint string `json:"endpoint"`
			}
			if !decodePushBody(w, r, &body) {
				return
			}
			s.pushNoContent(w, r, s.RoomPush.DeleteSubscription(appSlug, id, body.Endpoint))
		case http.MethodGet:
			subs, err := s.RoomPush.ListSubscriptions(appSlug, id)
			if err != nil {
				s.writePushError(w, r, err)
				return
			}
			writePushJSON(w, http.StatusOK, subs)
		default:
			roomMethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		}
	case "/schedule":
		switch r.Method {
		case http.MethodPut:
			var sched domain.PushSchedule
			if !decodePushBody(w, r, &sched) {
				return
			}
			s.pushNoContent(w, r, s.RoomPush.PutSchedule(appSlug, id, sched))
		case http.MethodGet:
			sched, err := s.RoomPush.GetSchedule(appSlug, id)
			if err != nil {
				s.writePushError(w, r, err)
				return
			}
			writePushJSON(w, http.StatusOK, sched)
		default:
			roomMethodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
	case "/test":
		if r.Method != http.MethodPost {
			roomMethodNotAllowed(w, http.MethodPost)
			return
		}
		res, err := s.RoomPush.Test(appSlug, id)
		if err != nil {
			s.writePushError(w, r, err)
			return
		}
		writePushJSON(w, http.StatusAccepted, res)
	}
}

// decodePushBody decodes one JSON document under pushMaxBodyBytes, writing
// the 400 itself on failure.
func decodePushBody(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, pushMaxBodyBytes))
	if err := dec.Decode(out); err != nil {
		http.Error(w, "malformed JSON body\n", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) pushNoContent(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		s.writePushError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writePushJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writePushError maps the push sentinels: validation is 400 with the detail,
// a missing room the existence-not-leaked 404, the subscription cap 413, the
// test interval 429 with Retry-After in whole seconds, rounded up.
func (s *Server) writePushError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrPushInvalid):
		http.Error(w, err.Error()+"\n", http.StatusBadRequest)
	case errors.Is(err, service.ErrRoomNotFound):
		http.NotFound(w, r)
	case errors.Is(err, service.ErrPushCap):
		http.Error(w, "room is at its push subscription cap\n", http.StatusRequestEntityTooLarge)
	case errors.Is(err, service.ErrPushRateLimited):
		secs := int(domain.PushTestInterval.Seconds())
		if rl, ok := errors.AsType[*service.PushRateLimit](err); ok && rl.RetryAfter > 0 {
			secs = int(math.Ceil(rl.RetryAfter.Seconds()))
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, "push test rate limit reached\n", http.StatusTooManyRequests)
	default:
		http.Error(w, "internal error\n", http.StatusInternalServerError)
	}
}

// roomPushSubpath reports whether key is the reserved push segment and
// returns the remainder after it ("" for the bare key).
func roomPushSubpath(key string) (string, bool) {
	if key == domain.RoomKeyPush {
		return "", true
	}
	if rest, ok := strings.CutPrefix(key, domain.RoomKeyPush+"/"); ok {
		return "/" + rest, true
	}
	return "", false
}
