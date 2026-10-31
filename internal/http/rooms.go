package http

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// RoomService is the application-service surface the HTTP layer needs for the
// /api/rooms endpoints. internal/service.Rooms satisfies it. Optional: when
// nil the /api/rooms prefix is not served.
type RoomService interface {
	Create(appSlug domain.Slug, subnet string) (domain.Room, error)
	Get(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error)
	// Scan returns the namespace stamped with the exact per-room sequence its
	// state reflects: the relay's late-join snapshot fence.
	Scan(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error)
	// Put / Delete return the mutation's assigned per-room sequence, which the
	// handlers stamp onto the live mirror frame (SPEC.md "The wire format: seq
	// on every durable frame").
	Put(appSlug domain.Slug, id domain.RoomID, key string, val []byte) (uint64, error)
	Delete(appSlug domain.Slug, id domain.RoomID, key string) (uint64, error)
}

// roomAPIPrefix is carved out of the site's path space on the app's own
// subdomain: a site's manifest lookup never serves a file here, so the API
// path and the static-file path cannot collide.
const roomAPIPrefix = "/api/rooms"

// roomMaxBodyBytes bounds a PUT body. A body this large is already over the
// per-room cap, so the service layer surfaces the precise 413.
const roomMaxBodyBytes = domain.MaxRoomValueBytes + 1

// roomAPIPath reports whether reqPath addresses the rooms API and returns the
// remainder after roomAPIPrefix. It matches the prefix exactly or as a whole
// path segment, but NOT a longer first segment like "/api/roomsX", which is a
// normal site path.
func roomAPIPath(reqPath string) (rest string, ok bool) {
	if reqPath == roomAPIPrefix {
		return "", true
	}
	if strings.HasPrefix(reqPath, roomAPIPrefix+"/") {
		return reqPath[len(roomAPIPrefix):], true
	}
	return "", false
}

// handleRoomsAPI serves the /api/rooms surface for appSlug. apiPath is the
// request path with roomAPIPrefix stripped: "" / "/" is the collection,
// "/<uuid>" is a room, "/<uuid>/<key>" is one value. Returns true once it has
// handled the request. Runs BEFORE the static-site lookup so a manifest file
// can never shadow the API prefix; room CREATION additionally requires a live
// app (see createRoom).
func (s *Server) handleRoomsAPI(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, apiPath string) bool {
	// Room values change under fixed URLs, so no response here may be stored,
	// errors included: without a directive a CDN caches a 404 by its own default.
	w.Header().Set("Cache-Control", "no-store")
	if s.Rooms == nil {
		http.NotFound(w, r)
		return true
	}

	rest := strings.TrimPrefix(apiPath, "/")

	if rest == "" {
		if r.Method != http.MethodPost {
			roomMethodNotAllowed(w, http.MethodPost)
			return true
		}
		s.createRoom(w, r, appSlug)
		return true
	}

	// /<uuid>            -> scan the room (GET)
	// /<uuid>/<key...>   -> one value (GET / PUT / DELETE)
	idStr := rest
	key := ""
	if before, after, ok := strings.Cut(rest, "/"); ok {
		idStr = before
		key = after
	}

	id, err := domain.ParseRoomID(idStr)
	if err != nil {
		// A malformed UUID is a 400 ("not a room id at all"), distinct from
		// the 404 a well-formed-but-nonexistent room gets. Neither confirms
		// the existence of any specific room.
		http.Error(w, "malformed room id\n", http.StatusBadRequest)
		return true
	}

	if key == "" {
		if r.Method != http.MethodGet {
			roomMethodNotAllowed(w, http.MethodGet)
			return true
		}
		s.scanRoom(w, r, appSlug, id)
		return true
	}

	// "ws" is the one key the KV verbs do not serve as data: it is the
	// real-time relay upgrade, carved out BEFORE the value verbs so they never
	// see it. A non-Upgrade GET here is refused by websocket.Accept.
	if key == wsKey {
		s.handleRoomWS(w, r, appSlug, id)
		return true
	}
	// "push" and everything under "push/" is the push surface, carved out the
	// same way.
	if sub, ok := roomPushSubpath(key); ok {
		s.handleRoomPush(w, r, appSlug, id, sub)
		return true
	}

	switch r.Method {
	case http.MethodGet:
		s.getRoomValue(w, r, appSlug, id, key)
	case http.MethodPut:
		s.putRoomValue(w, r, appSlug, id, key)
	case http.MethodDelete:
		s.deleteRoomValue(w, r, appSlug, id, key)
	default:
		roomMethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
	return true
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request, appSlug domain.Slug) {
	// The slug must name a LIVE app before a room can be created under it.
	// Without this, any well-formed 8-char slug (~10^12 of them) could host
	// rooms, each with its own per-app creation and byte budget, so an
	// attacker rotating slugs would defeat both per-app caps. An unknown slug
	// 404s, the same existence-not-leaked shape a missing room gets.
	if !s.appExists(appSlug) {
		http.NotFound(w, r)
		return
	}
	subnet := clientSubnet(r, s.ClientIP)
	room, err := s.Rooms.Create(appSlug, subnet)
	if err != nil {
		s.writeRoomError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(struct {
		ID string `json:"id"`
	}{ID: room.ID.String()})
}

// appExists reports whether appSlug names a site or paste. A read error or a
// not-found is "no such app". With neither reader wired nothing resolves, so
// creation is refused rather than allowed unbounded.
func (s *Server) appExists(appSlug domain.Slug) bool {
	if s.Sites != nil {
		if _, err := s.Sites.Get(appSlug); err == nil {
			return true
		}
	}
	if s.Pastes != nil {
		if _, err := s.Pastes.Get(appSlug); err == nil {
			return true
		}
	}
	return false
}

func (s *Server) scanRoom(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID) {
	kv, err := s.Rooms.Scan(appSlug, id)
	if err != nil {
		s.writeRoomError(w, r, err)
		return
	}
	// One JSON object, keys sorted as json.Encoder sorts a map's, each value
	// the stored text written as is: an encoder would compact it and escape
	// HTML, and the frames carry it unchanged.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, "{")
	for i, k := range slices.Sorted(maps.Keys(kv.Values)) {
		name, _ := json.Marshal(k)
		if i > 0 {
			_, _ = io.WriteString(w, ",")
		}
		_, _ = w.Write(name)
		_, _ = io.WriteString(w, ":")
		_, _ = w.Write(kv.WireValue(k))
	}
	_, _ = io.WriteString(w, "}\n")
}

func (s *Server) getRoomValue(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID, key string) {
	if err := domain.ValidateRoomKey(key); err != nil {
		http.Error(w, "invalid key\n", http.StatusBadRequest)
		return
	}
	val, err := s.Rooms.Get(appSlug, id, key)
	if err != nil {
		s.writeRoomError(w, r, err)
		return
	}
	// application/json only when the stored bytes are recognizably JSON, else
	// application/octet-stream, so an app's opaque bytes are never mislabeled
	// as something a browser would execute.
	if domain.RoomValueIsJSON(val) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	_, _ = w.Write(val)
}

func (s *Server) putRoomValue(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID, key string) {
	if err := domain.ValidateRoomKey(key); err != nil {
		http.Error(w, "invalid key\n", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, roomMaxBodyBytes))
	if err != nil {
		http.Error(w, "read body\n", http.StatusBadRequest)
		return
	}
	// The live mirror is NOT built here: the room cell broadcasts the frame in
	// the same event that commits the write and assigns its seq, so mirroring
	// from this pod would deliver every frame twice.
	_, err = s.Rooms.Put(appSlug, id, key, body)
	if err != nil {
		s.writeRoomError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRoomValue(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, id domain.RoomID, key string) {
	if err := domain.ValidateRoomKey(key); err != nil {
		http.Error(w, "invalid key\n", http.StatusBadRequest)
		return
	}
	// See putRoomValue: the cell mirrors its own commits.
	var err error
	_, err = s.Rooms.Delete(appSlug, id, key)
	if err != nil {
		s.writeRoomError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRoomError maps the service-layer room sentinels to status codes: a
// not-found (room OR key) is the existence-not-leaked 404, a rate limit is 429
// with Retry-After, a per-room cap is 413, a per-app aggregate is 507.
func (s *Server) writeRoomError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrRoomNotFound):
		http.NotFound(w, r)
	case errors.Is(err, service.ErrRoomCreateRateLimited):
		if rl, ok := errors.AsType[*service.RoomRateLimit](err); ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.Window.Seconds())))
		}
		http.Error(w, "room creation rate limit reached\n", http.StatusTooManyRequests)
	case errors.Is(err, service.ErrRoomDataCap):
		http.Error(w, "room is at its data cap\n", http.StatusRequestEntityTooLarge)
	case errors.Is(err, service.ErrAppRoomsCap):
		http.Error(w, "app room storage is at capacity\n", http.StatusInsufficientStorage)
	default:
		http.Error(w, "internal error\n", http.StatusInternalServerError)
	}
}

// roomMethodNotAllowed writes a 405 with the Allow header listing the
// methods the route accepts.
func roomMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	http.Error(w, "method not allowed\n", http.StatusMethodNotAllowed)
}
