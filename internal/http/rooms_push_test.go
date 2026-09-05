package http

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func pushSubJSON(n int) []byte {
	p := make([]byte, 65)
	p[0] = 0x04
	b, _ := json.Marshal(map[string]any{
		"endpoint":       fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", n),
		"expirationTime": nil,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(p),
			"auth":   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		},
	})
	return b
}

// futureWhen is a one-shot instant a day ahead of the wall clock the service
// validates against.
var futureWhen = time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)

var scheduleJSON = `{"tz":"America/New_York","items":[
  {"id":"morning","at":"08:00","days":[1,2,3,4,5,6],"title":"Rota","bodyKey":"push:{date}","url":"/?room=x#/","tag":"rota-today"},
  {"id":"once","when":"` + futureWhen + `","title":"Rota","body":"Deep clean"}]}`

func TestPushHTTP_Key(t *testing.T) {
	srv := buildRoomServer(t)
	w := req(t, srv, http.MethodGet, "appz2345", "/api/push/key", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("key: code %d body %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type %q", ct)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(out.Key)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("key %q: len %d err %v", out.Key, len(raw), err)
	}
	// Stable across calls, and the same key in path mode.
	w = req(t, srv, http.MethodGet, "appz2345", "/api/push/key", nil)
	var again struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &again)
	if again.Key != out.Key {
		t.Fatalf("key changed between calls")
	}
	r := httptest.NewRequest(http.MethodGet, "http://hostthis.test/p/appz2345/api/push/key", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != http.StatusOK || again.Key != out.Key {
		t.Fatalf("path mode: code %d key %q", rec.Code, again.Key)
	}

	w = req(t, srv, http.MethodPost, "appz2345", "/api/push/key", nil)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST key: code %d allow %q", w.Code, w.Header().Get("Allow"))
	}
	// A slug with no app never hands out a key.
	srv.Sites = nil
	w = req(t, srv, http.MethodGet, "appz2345", "/api/push/key", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown app key: code %d", w.Code)
	}
}

func TestPushHTTP_Subscriptions(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)
	path := "/api/rooms/" + id + "/push/subscriptions"

	w := req(t, srv, http.MethodPut, slug, path, pushSubJSON(1))
	if w.Code != http.StatusNoContent {
		t.Fatalf("put: code %d body %q", w.Code, w.Body.String())
	}
	// Same endpoint again refreshes rather than duplicates.
	w = req(t, srv, http.MethodPut, slug, path, pushSubJSON(1))
	if w.Code != http.StatusNoContent {
		t.Fatalf("put again: code %d", w.Code)
	}
	w = req(t, srv, http.MethodGet, slug, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: code %d", w.Code)
	}
	var list []struct {
		Endpoint string `json:"endpoint"`
		Added    string `json:"added"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%q)", err, w.Body.String())
	}
	if len(list) != 1 || list[0].Endpoint != "https://fcm.googleapis.com/fcm/send/1" {
		t.Fatalf("list = %+v", list)
	}
	if _, err := time.Parse(time.RFC3339, list[0].Added); err != nil {
		t.Fatalf("added %q not RFC 3339: %v", list[0].Added, err)
	}
	// Keys never come back.
	if strings.Contains(w.Body.String(), "p256dh") {
		t.Fatalf("list leaks keys: %s", w.Body.String())
	}

	w = req(t, srv, http.MethodDelete, slug, path, []byte(`{"endpoint":"https://fcm.googleapis.com/fcm/send/1"}`))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: code %d body %q", w.Code, w.Body.String())
	}
	w = req(t, srv, http.MethodGet, slug, path, nil)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("list after delete = %q, want []", w.Body.String())
	}

	// 400s: invalid subscription, malformed JSON, delete without endpoint.
	bad := strings.Replace(string(pushSubJSON(2)), "https://", "http://", 1)
	offList := strings.Replace(string(pushSubJSON(2)), "fcm.googleapis.com", "push.example.test", 1)
	for name, body := range map[string][]byte{
		"http endpoint":     []byte(bad),
		"unknown push host": []byte(offList),
		"not json":          []byte("{"),
		"empty":             []byte(""),
	} {
		w = req(t, srv, http.MethodPut, slug, path, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: code %d, want 400", name, w.Code)
		}
	}
	w = req(t, srv, http.MethodDelete, slug, path, []byte(`{}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("delete without endpoint: code %d", w.Code)
	}
	w = req(t, srv, http.MethodPatch, slug, path, nil)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") == "" {
		t.Fatalf("PATCH: code %d allow %q", w.Code, w.Header().Get("Allow"))
	}

	// The cap: the 17th distinct endpoint is a 413; refreshing an existing one still works.
	for i := range domain.MaxPushSubscriptions {
		if w = req(t, srv, http.MethodPut, slug, path, pushSubJSON(100+i)); w.Code != http.StatusNoContent {
			t.Fatalf("put %d: code %d", i, w.Code)
		}
	}
	w = req(t, srv, http.MethodPut, slug, path, pushSubJSON(999))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over cap: code %d, want 413", w.Code)
	}
	w = req(t, srv, http.MethodPut, slug, path, pushSubJSON(100))
	if w.Code != http.StatusNoContent {
		t.Fatalf("refresh at cap: code %d", w.Code)
	}
}

func TestPushHTTP_Schedule(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)
	path := "/api/rooms/" + id + "/push/schedule"

	w := req(t, srv, http.MethodGet, slug, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get unset: code %d", w.Code)
	}
	var sched domain.PushSchedule
	if err := json.Unmarshal(w.Body.Bytes(), &sched); err != nil || sched.Items == nil || len(sched.Items) != 0 {
		t.Fatalf("unset schedule = %q (%v), want empty items array", w.Body.String(), err)
	}

	w = req(t, srv, http.MethodPut, slug, path, []byte(scheduleJSON))
	if w.Code != http.StatusNoContent {
		t.Fatalf("put: code %d body %q", w.Code, w.Body.String())
	}
	w = req(t, srv, http.MethodGet, slug, path, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &sched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sched.TZ != "America/New_York" || len(sched.Items) != 2 || sched.Items[0].BodyKey != "push:{date}" ||
		sched.Items[1].When != futureWhen || len(sched.Items[0].Days) != 6 {
		t.Fatalf("schedule round trip = %+v", sched)
	}

	// Invalid documents are refused whole: the stored one is untouched.
	invalid := strings.Replace(scheduleJSON, `"id":"once"`, `"id":"morning"`, 1)
	w = req(t, srv, http.MethodPut, slug, path, []byte(invalid))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate id: code %d", w.Code)
	}
	w = req(t, srv, http.MethodPut, slug, path, []byte(`{"tz":"Mars/Base","items":[]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad tz: code %d", w.Code)
	}
	w = req(t, srv, http.MethodPut, slug, path, []byte(`{"items":[]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("clear without tz: code %d", w.Code)
	}
	past := strings.Replace(scheduleJSON, futureWhen, "2020-01-01T08:00:00Z", 1)
	w = req(t, srv, http.MethodPut, slug, path, []byte(past))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("past when: code %d", w.Code)
	}
	wide := strings.Replace(scheduleJSON, `"title":"Rota","body"`, `"title":"`+strings.Repeat("\u00e9", 64)+`","body"`, 1)
	w = req(t, srv, http.MethodPut, slug, path, []byte(wide))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("64-rune title over 64 bytes: code %d", w.Code)
	}
	w = req(t, srv, http.MethodPut, slug, path, []byte(`not json`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("not json: code %d", w.Code)
	}
	w = req(t, srv, http.MethodGet, slug, path, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &sched)
	if len(sched.Items) != 2 {
		t.Fatalf("invalid PUT altered schedule: %+v", sched)
	}

	// Empty items clears.
	w = req(t, srv, http.MethodPut, slug, path, []byte(`{"tz":"UTC","items":[]}`))
	if w.Code != http.StatusNoContent {
		t.Fatalf("clear: code %d", w.Code)
	}
	w = req(t, srv, http.MethodGet, slug, path, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &sched)
	if len(sched.Items) != 0 {
		t.Fatalf("not cleared: %+v", sched)
	}
	w = req(t, srv, http.MethodDelete, slug, path, nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE schedule: code %d", w.Code)
	}
}

func TestPushHTTP_Test(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)
	path := "/api/rooms/" + id + "/push/test"

	w := req(t, srv, http.MethodPost, slug, path, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("test: code %d body %q", w.Code, w.Body.String())
	}
	var res struct {
		Sent   *int `json:"sent"`
		Pruned *int `json:"pruned"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Sent == nil || res.Pruned == nil {
		t.Fatalf("test body %q: %v", w.Body.String(), err)
	}
	w = req(t, srv, http.MethodPost, slug, path, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second test: code %d, want 429", w.Code)
	}
	ra, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	w = req(t, srv, http.MethodGet, slug, path, nil)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET test: code %d allow %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestPushHTTP_RoomErrors(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	missing := domain.NewRoomID().String()
	for _, tc := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPut, "/api/rooms/" + missing + "/push/subscriptions", pushSubJSON(1)},
		{http.MethodDelete, "/api/rooms/" + missing + "/push/subscriptions", []byte(`{"endpoint":"https://x/y"}`)},
		{http.MethodGet, "/api/rooms/" + missing + "/push/subscriptions", nil},
		{http.MethodPut, "/api/rooms/" + missing + "/push/schedule", []byte(scheduleJSON)},
		{http.MethodGet, "/api/rooms/" + missing + "/push/schedule", nil},
		{http.MethodPost, "/api/rooms/" + missing + "/push/test", nil},
	} {
		w := req(t, srv, tc.method, slug, tc.path, tc.body)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: code %d, want 404", tc.method, tc.path, w.Code)
		}
	}
	w := req(t, srv, http.MethodGet, slug, "/api/rooms/not-a-uuid/push/schedule", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: code %d", w.Code)
	}
	// Without an app, the push routes are as absent as the rest.
	id := createRoomID(t, srv, slug)
	srv.Sites = nil
	w = req(t, srv, http.MethodGet, slug, "/api/rooms/"+id+"/push/schedule", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no app: code %d", w.Code)
	}
}

func TestPushHTTP_DisabledIs404(t *testing.T) {
	srv := buildRoomServer(t)
	srv.RoomPush = nil
	id := createRoomID(t, srv, "appz2345")
	for _, path := range []string{"/api/push/key", "/api/rooms/" + id + "/push/schedule"} {
		if w := req(t, srv, http.MethodGet, "appz2345", path, nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s: code %d, want 404", path, w.Code)
		}
	}
}

func TestPushHTTP_ReservedKeys(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)
	for _, key := range []string{"push", "push/", "push/other", "push/subscriptions/x"} {
		for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodDelete} {
			w := req(t, srv, method, slug, "/api/rooms/"+id+"/"+key, []byte("v"))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s %q: code %d, want 400", method, key, w.Code)
			}
		}
	}
	// With no app behind the slug the reserved shapes 404 like everything
	// else under it: the 400 must not reveal that the room API is live.
	srv.Sites = nil
	for _, key := range []string{"push", "push/", "push/other", "push/subscriptions/x"} {
		w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/"+key, []byte("v"))
		if w.Code != http.StatusNotFound {
			t.Errorf("no app: PUT %q: code %d, want 404", key, w.Code)
		}
	}
	srv.Sites = liveAppSiteReader{}
	// Other shapes stay ordinary data.
	for _, key := range []string{"push:2026-09-06", "pushy"} {
		w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/"+key, []byte("v"))
		if w.Code != http.StatusNoContent {
			t.Errorf("PUT %q: code %d, want 204", key, w.Code)
		}
	}
	w := req(t, srv, http.MethodGet, slug, "/api/rooms/"+id, nil)
	var scan map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &scan)
	if len(scan) != 2 {
		t.Fatalf("scan = %v, want the 2 data keys only", scan)
	}
}
