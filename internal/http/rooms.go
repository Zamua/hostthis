package http

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// RoomService is the application-service surface the HTTP layer needs for
// the /api/rooms endpoints. internal/service.Rooms satisfies it.
// Optional: when nil, the /api/rooms prefix is not served (the metadata
// backend has no room repo wired).
type RoomService interface {
	Create(appSlug domain.Slug, subnet string) (domain.Room, error)
	Get(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error)
	Scan(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error)
	Put(appSlug domain.Slug, id domain.RoomID, key string, val []byte) error
	Delete(appSlug domain.Slug, id domain.RoomID, key string) error
}

// roomAPIPrefix is the reserved path the rooms API lives under, on the
// app's own subdomain. It is carved out of the site's path space: a
// site's manifest lookup never serves a file here, so the API path and
// the static-file path do not collide.
const roomAPIPrefix = "/api/rooms"

// roomMaxBodyBytes bounds a single PUT body. A value larger than the
// whole-room cap can never fit (the domain CanPut rejects it), so we cap
// the request body at the room byte cap + a small slack and let the
// service layer surface the precise 413. Reading is bounded so a hostile
// client cannot stream an unbounded body into memory.
const roomMaxBodyBytes = domain.MaxRoomValueBytes + 1

// roomAPIPath reports whether reqPath addresses the rooms API and, if
// so, returns the remainder after roomAPIPrefix. It matches the prefix
// exactly ("/api/rooms" -> "") or as a path segment ("/api/rooms/<rest>"
// -> "/<rest>"), but NOT a longer first segment like "/api/roomsX",
// which is a normal site path. This keeps the carve-out tight: only the
// reserved prefix is intercepted, everything else falls through to the
// static-site / paste path.
func roomAPIPath(reqPath string) (rest string, ok bool) {
	if reqPath == roomAPIPrefix {
		return "", true
	}
	if strings.HasPrefix(reqPath, roomAPIPrefix+"/") {
		return reqPath[len(roomAPIPrefix):], true
	}
	return "", false
}

// handleRoomsAPI serves the /api/rooms surface for the app identified by
// appSlug. apiPath is the request path with the roomAPIPrefix already
// stripped (so "" / "/" is the collection, "/<uuid>" is a room,
// "/<uuid>/<key>" is one value). Returns true once it has handled the
// request; the caller must then return.
//
// The router calls this BEFORE the static-site lookup so the API prefix
// is never shadowed by a manifest file. A request whose app slug owns a
// site OR a live paste gets the rooms API (both are valid apps); room
// CREATION additionally requires the slug to name a live app (see
// createRoom's existence gate) so an unprovisioned slug 404s rather than
// minting a fresh per-app budget.
func (s *Server) handleRoomsAPI(w http.ResponseWriter, r *http.Request, appSlug domain.Slug, apiPath string) bool {
	if s.Rooms == nil {
		// Rooms not wired on this backend: 404 the whole prefix.
		http.NotFound(w, r)
		return true
	}

	rest := strings.TrimPrefix(apiPath, "/")

	// Collection: POST /api/rooms -> create.
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
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		idStr = rest[:i]
		key = rest[i+1:]
	}

	id, err := domain.ParseRoomID(idStr)
	if err != nil {
		// A malformed UUID is a 400 - "this is not a room id at all" -
		// distinct from the 404 a well-formed-but-nonexistent room gets.
		// Neither confirms the existence of any specific room.
		http.Error(w, "malformed room id\n", http.StatusBadRequest)
		return true
	}

	if key == "" {
		// Whole-room scan.
		if r.Method != http.MethodGet {
			roomMethodNotAllowed(w, http.MethodGet)
			return true
		}
		s.scanRoom(w, r, appSlug, id)
		return true
	}

	// Single value verbs.
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
	// The slug must name a LIVE app (a non-expired site or paste) before a
	// room can be created under it. Without this, any well-formed 8-char
	// slug (~10^12 of them) could host rooms, each getting its own
	// per-app creation + byte budget - so an attacker rotating slugs would
	// defeat both per-app caps. Tying creation to an operator-provisioned
	// app bounds the per-app caps to a finite set. An unknown slug 404s,
	// the same existence-not-leaked shape a missing room gets.
	if !s.appExists(appSlug) {
		http.NotFound(w, r)
		return
	}
	subnet := clientSubnet(r)
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

// appExists reports whether appSlug names a LIVE (non-expired) site or
// paste - the existence requirement room creation rides on. A read error
// or a not-found from either reader is "no such app." An expired site /
// paste is treated as gone (the same line the read handlers draw). When
// neither reader is wired, no app can be resolved, so creation is refused
// (404) rather than allowed unbounded.
func (s *Server) appExists(appSlug domain.Slug) bool {
	now := s.nowOrTime()
	if s.Sites != nil {
		if site, err := s.Sites.Get(appSlug); err == nil && now.Before(site.ExpiresAt) {
			return true
		}
	}
	if s.Pastes != nil {
		if paste, err := s.Pastes.Get(appSlug); err == nil && now.Before(paste.ExpiresAt) {
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
	// Encode the whole namespace as one JSON object: key -> value, with
	// each value embedded as raw JSON when it parses as JSON, else as a
	// JSON string of the verbatim bytes. The app loads full state in one
	// request on join.
	out := make(map[string]json.RawMessage, kv.KeyCount())
	for k, v := range kv.Values {
		out[k] = jsonValue(v)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
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
	// Conservative content type: application/json only when the stored
	// bytes are recognizably JSON, else application/octet-stream so an
	// app's opaque bytes are never mislabeled as something a browser
	// would execute.
	w.Header().Set("Cache-Control", "no-store")
	if isJSON(val) {
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
	// Bound the body. ReadAll on a capped reader returns at most
	// roomMaxBodyBytes; a body at exactly the limit+1 is over the
	// per-room cap and the service layer 413s it.
	body, err := io.ReadAll(io.LimitReader(r.Body, roomMaxBodyBytes))
	if err != nil {
		http.Error(w, "read body\n", http.StatusBadRequest)
		return
	}
	if err := s.Rooms.Put(appSlug, id, key, body); err != nil {
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
	if err := s.Rooms.Delete(appSlug, id, key); err != nil {
		s.writeRoomError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRoomError maps the service-layer room sentinels to status codes.
// A not-found (room OR key) is 404 - the existence-not-leaked shape; a
// rate-limit is 429 with Retry-After; a per-room cap is 413; a per-app
// aggregate is 507.
func (s *Server) writeRoomError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrRoomNotFound):
		http.NotFound(w, r)
	case errors.Is(err, service.ErrRoomCreateRateLimited):
		var rl *service.RoomRateLimit
		if errors.As(err, &rl) {
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

// trustXFFEnv reports whether the operator has opted into trusting the
// X-Forwarded-For header for client-IP derivation. Read fresh from the
// environment so tests can flip it per-case via t.Setenv. Mirrors the SSH
// side's HOSTTHIS_SSH_PROXY_PROTOCOL discipline: a client-supplied header
// is NEVER trusted by default, only when an operator behind a reverse
// proxy explicitly enables it.
func trustXFFEnv() bool {
	return strings.EqualFold(os.Getenv("HOSTTHIS_HTTP_TRUST_XFF"), "true")
}

// clientSubnet derives the canonical /24 (IPv4) or /48 (IPv6) subnet of
// the requester for the per-IP room-creation rate limit, mirroring the
// shape the SSH Sybil gate uses.
//
// By DEFAULT the address comes from the TCP RemoteAddr only - a
// client-supplied X-Forwarded-For is ignored. Trusting XFF by default
// would be a rate-limit bypass: an attacker could set a fresh
// X-Forwarded-For per POST and land in a new per-IP bucket each time,
// exactly the inconsistency the SSH Sybil gate avoids by using only the
// real RemoteAddr (or an env-gated PROXY-protocol header).
//
// When the operator sets HOSTTHIS_HTTP_TRUST_XFF=true (because hostthis
// sits behind a reverse proxy that appends the real client IP), the
// RIGHT-MOST X-Forwarded-For value is used - the hop the trusted proxy
// itself recorded, which the client cannot forge past the proxy - not the
// left-most value, which is fully attacker-controlled. An unparseable
// address becomes the stable "unknown" bucket rather than crashing.
func clientSubnet(r *http.Request) string {
	host := ""
	if trustXFFEnv() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Take the RIGHT-MOST hop: behind a trusted proxy, the proxy
			// appends the real client IP as the last entry, so the right-most
			// value is the proxy's own view of the client. The left-most
			// entries are whatever the client chose to send and must not be
			// trusted.
			parts := strings.Split(xff, ",")
			host = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if host == "" {
		h, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		} else {
			host = h
		}
	}
	ip := net.ParseIP(host)
	return ipSubnet(ip)
}

// ipSubnet returns the canonical subnet string. IPv4 -> "/24", IPv6 ->
// "/48". A nil IP becomes "unknown" so the rate limit treats it as one
// stable bucket. Same derivation the SSH Sybil gate uses.
func ipSubnet(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

// jsonValue returns v as raw JSON when it already parses as JSON (so an
// app that stored a JSON value gets it back as structured JSON in a
// scan), else as a JSON string of the verbatim bytes (so opaque bytes
// round-trip without corrupting the scan's JSON object).
func jsonValue(v []byte) json.RawMessage {
	if isJSON(v) {
		return json.RawMessage(v)
	}
	encoded, err := json.Marshal(string(v))
	if err != nil {
		// Unreachable for any []byte; fall back to a JSON null.
		return json.RawMessage("null")
	}
	return json.RawMessage(encoded)
}

// isJSON reports whether b is syntactically valid JSON.
func isJSON(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return json.Valid(b)
}
