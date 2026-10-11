package parity

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// clients hands each room creation an address of its own /24, so no request
// meets the creation limit unless it means to.
var clients atomic.Int64

func nextClient() string {
	n := clients.Add(1)
	return fmt.Sprintf("10.%d.%d.1", n/256%256, n%256)
}

// roomApp is the app the room checks write under: a paste of their own
// locally, the dedicated test paste against a deployment.
func (w *world) roomApp(t *testing.T) string {
	t.Helper()
	if !w.local {
		return w.writeApp
	}
	return w.upload(t, "--type html", []byte("<!doctype html><title>app</title>\n"))
}

// createRoom makes a room through s and returns its id.
func (w *world) createRoom(t *testing.T, s *server, host string) string {
	t.Helper()
	res, err := s.do(request{method: "POST", host: host, target: "/api/rooms", client: nextClient()})
	if err != nil || res.status != 201 || !mintedRoom(res.body) {
		t.Fatalf("create a room through %s: %v %d %q", s.name, err, res.status, res.body)
	}
	v, _ := decodeJSON(res.body)
	return v.(map[string]any)["id"].(string)
}

func mustDo(t *testing.T, s *server, r request, want int) response {
	t.Helper()
	res, err := s.do(r)
	if err != nil || res.status != want {
		t.Fatalf("%s through %s: %v %d %q, want %d", r, s.name, err, res.status, res.body, want)
	}
	return res
}

// roomValues are stored values whose encodings differ: JSON whose numbers and
// spacing must survive, strings with characters each server escapes its own
// way, bytes that are not UTF-8, a byte-order mark, nesting past json.Valid's
// limit, and keys that need escaping in a URL and in JSON.
var roomValues = []struct{ key, value string }{
	{"json", `{"b": 1, "a": [1.50, -0, 1e400, 12345678901234567890]}`},
	{"html<&>", `"<script>&amp;</script>"`},
	{"text", "plain text, not JSON"},
	{"invalid-utf8", "\xff\xfe\xc3(abc"},
	{"bom", "\xef\xbb\xbf{\"x\":1}"},
	{"empty", ""},
	{"deep", strings.Repeat("[", 10001) + strings.Repeat("]", 10001)},
	{"separators", "\"\u2028\u2029\""},
	{"number", "42"},
	{"k<\u00e9>&\u2028", `{"key":"needs escaping"}`},
	{"a/b", `"a key with a slash"`},
	{"unicode \u2713", `"check"`},
}

func keyTarget(id, key string) string {
	return "/api/rooms/" + id + "/" + url.PathEscape(key)
}

func testRooms(t *testing.T, w *world) {
	if !w.writable {
		t.Skip("read-only: room checks write; set PARITY_ALLOW_WRITES=1 and PARITY_WRITE_SLUG")
	}
	app := w.roomApp(t)
	host := w.host(app)
	writer := w.workers[0]
	missing := domain.NewRoomID().String()
	never := w.host(domain.NewRandomSlug().String())

	t.Run("collection", func(t *testing.T) {
		w.expect(t, request{method: "POST", host: host, target: "/api/rooms", client: nextClient()}, 201)
		w.expect(t, request{method: "POST", host: never, target: "/api/rooms", client: nextClient()}, 404)
		for _, m := range []string{"GET", "PUT", "DELETE", "HEAD"} {
			w.check(t, request{method: m, host: host, target: "/api/rooms"})
		}
		w.check(t, get(host, "/api/rooms/"))
		w.check(t, get(host, "/api/roomsX"))
	})

	// Room a is written through hostthisd and room b through the Worker, so
	// each server reads values the other stored.
	a := w.createRoom(t, w.hostthisd, host)
	b := w.createRoom(t, writer, host)
	for _, v := range roomValues {
		mustDo(t, w.hostthisd, request{method: "PUT", host: host, target: keyTarget(a, v.key), body: []byte(v.value)}, 204)
		mustDo(t, writer, request{method: "PUT", host: host, target: keyTarget(b, v.key), body: []byte(v.value)}, 204)
	}

	t.Run("reads", func(t *testing.T) {
		for _, id := range []string{a, b, strings.ToUpper(a)} {
			w.check(t, get(host, "/api/rooms/"+id))
			for _, v := range roomValues {
				w.check(t, get(host, keyTarget(id, v.key)))
			}
			w.expect(t, get(host, keyTarget(id, "absent")), 404)
			w.check(t, get(host, "/api/rooms/"+id+"/a%2Fb"))
		}
		for _, target := range []string{
			"/api/rooms/" + missing, keyTarget(missing, "k"), "/api/rooms/not-a-uuid", "/api/rooms/not-a-uuid/k",
			"/api/rooms/6ba7b810-9dad-11d1-80b4-00c04fd430c8", "/api/rooms/" + a + "/",
		} {
			w.check(t, get(host, target))
		}
		w.check(t, get(never, "/api/rooms/"+a))
		if w.local {
			// Another live app holds no room of this one.
			other := w.host(w.roomApp(t))
			w.expect(t, get(other, "/api/rooms/"+a), 404)
			w.expect(t, get(other, keyTarget(a, "json")), 404)
			w.expect(t, request{method: "PUT", host: other, target: keyTarget(a, "json"), body: []byte("x")}, 404)
		}
		w.check(t, get(never, keyTarget(a, "json")))
	})

	t.Run("writes", func(t *testing.T) {
		c := w.createRoom(t, w.hostthisd, host)
		for _, v := range roomValues {
			w.check(t, request{method: "PUT", host: host, target: keyTarget(c, v.key), body: []byte(v.value)})
		}
		w.check(t, get(host, "/api/rooms/"+c))
		bad := []string{
			"/api/rooms/" + c + "/" + strings.Repeat("k", 257), "/api/rooms/" + c + "/k%FF", "/api/rooms/" + c + "/%C0%80",
			"/api/rooms/" + c + "/push", "/api/rooms/" + c + "/push/x", keyTarget(missing, "k"), "/api/rooms/nope/k",
		}
		for _, target := range append(bad, "/api/rooms/"+c+"/"+strings.Repeat("k", 256)) {
			for _, m := range []string{"GET", "PUT", "DELETE"} {
				w.check(t, request{method: m, host: host, target: target, body: []byte("v")})
			}
		}
		for _, m := range []string{"POST", "PATCH", "HEAD"} {
			w.check(t, request{method: m, host: host, target: keyTarget(c, "json"), body: []byte("v")})
			w.check(t, request{method: m, host: host, target: "/api/rooms/" + c})
		}
		w.check(t, request{method: "DELETE", host: host, target: keyTarget(c, "json")})
		w.check(t, request{method: "DELETE", host: host, target: keyTarget(c, "never-written")})
		w.check(t, request{method: "DELETE", host: host, target: keyTarget(missing, "k")})
		w.check(t, get(host, "/api/rooms/"+c))
	})

	t.Run("room cap", func(t *testing.T) {
		d := w.createRoom(t, w.hostthisd, host)
		w.expect(t, request{method: "PUT", host: host, target: keyTarget(d, "big"), body: bytes.Repeat([]byte("x"), 256<<10+1)}, 413)
		mustDo(t, w.hostthisd, request{method: "PUT", host: host, target: keyTarget(d, "big"), body: bytes.Repeat([]byte("x"), 256<<10)}, 204)
		w.expect(t, request{method: "PUT", host: host, target: keyTarget(d, "one-more"), body: []byte("y")}, 413)
		w.expect(t, request{method: "PUT", host: host, target: keyTarget(d, "big"), body: bytes.Repeat([]byte("z"), 256<<10)}, 204)
		e := w.createRoom(t, w.hostthisd, host)
		for i := range domain.MaxRoomKeys {
			mustDo(t, w.hostthisd, request{method: "PUT", host: host, target: keyTarget(e, "k"+strconv.Itoa(i))}, 204)
		}
		w.expect(t, request{method: "PUT", host: host, target: keyTarget(e, "one-more"), body: []byte("y")}, 413)
		w.expect(t, request{method: "PUT", host: host, target: keyTarget(e, "k0"), body: []byte("y")}, 204)
	})

	t.Run("push", func(t *testing.T) { testPush(t, w, host, never, a) })

	if w.cell == nil {
		return
	}
	t.Run("creation limits", func(t *testing.T) {
		limited := w.roomApp(t)
		w.cell.fillRoomLedger(t, limited, "198.51.100.0/24", domain.MaxRoomsPerIPPerWindow)
		w.expect(t, request{method: "POST", host: w.host(limited), target: "/api/rooms", client: "198.51.100.9"}, 429)
		w.expect(t, request{method: "POST", host: w.host(limited), target: "/api/rooms", client: "198.51.101.9"}, 201)
		full := w.roomApp(t)
		w.cell.fillRoomLedger(t, full, "203.0.113.0/24", domain.MaxRoomsPerAppPerWindow)
		w.expect(t, request{method: "POST", host: w.host(full), target: "/api/rooms", client: "192.0.2.9"}, 429)
	})
	t.Run("app cap", func(t *testing.T) { testAppCap(t, w) })
}

// testAppCap fills an app's rooms to just under its aggregate cap with real
// writes, then crosses it from a room that still has room of its own.
func testAppCap(t *testing.T, w *world) {
	app := w.roomApp(t)
	host := w.host(app)
	const short = 100
	value := bytes.Repeat([]byte("v"), domain.MaxRoomBytes)
	rooms := int64(domain.MaxAppRoomBytes / domain.MaxRoomBytes)
	var wg sync.WaitGroup
	var claimed atomic.Int64
	var failed atomic.Bool
	for range 8 {
		wg.Go(func() {
			for n := claimed.Add(1); n <= rooms && !failed.Load(); n = claimed.Add(1) {
				id, ok := tryCreate(w.hostthisd, host)
				body := value
				if n == rooms {
					body = value[short:]
				}
				if !ok {
					failed.Store(true)
					return
				}
				res, err := w.hostthisd.do(request{method: "PUT", host: host, target: keyTarget(id, "v"), body: body})
				if err != nil || res.status != 204 {
					failed.Store(true)
					return
				}
			}
		})
	}
	wg.Wait()
	if failed.Load() {
		t.Fatal("could not fill the app's rooms")
	}
	id := w.createRoom(t, w.hostthisd, host)
	w.expect(t, request{method: "PUT", host: host, target: keyTarget(id, "k"), body: value[:short+1]}, 507)
	mustDo(t, w.hostthisd, request{method: "PUT", host: host, target: keyTarget(id, "k"), body: value[:short]}, 204)
	w.expect(t, request{method: "POST", host: host, target: "/api/rooms", client: nextClient()}, 507)
	w.expect(t, request{method: "PUT", host: host, target: keyTarget(id, "k2"), body: []byte("x")}, 507)
	// A replacement no larger than the value it replaces needs no capacity.
	w.expect(t, request{method: "PUT", host: host, target: keyTarget(id, "k"), body: value[:10]}, 204)
}

func tryCreate(s *server, host string) (string, bool) {
	res, err := s.do(request{method: "POST", host: host, target: "/api/rooms", client: nextClient()})
	if err != nil || res.status != 201 || !mintedRoom(res.body) {
		return "", false
	}
	v, _ := decodeJSON(res.body)
	return v.(map[string]any)["id"].(string), true
}

// A subscription a push service would accept the shape of. Nothing is sent
// to it: the rooms that hold one are never tested.
func subscription(endpoint string) string {
	point := append([]byte{4}, bytes.Repeat([]byte{7}, 64)...)
	return fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`, endpoint,
		base64.RawURLEncoding.EncodeToString(point), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16)))
}

func testPush(t *testing.T, w *world, host, never, room string) {
	missing := domain.NewRoomID().String()
	for _, h := range []string{host, never} {
		for _, m := range []string{"GET", "POST", "HEAD"} {
			w.check(t, request{method: m, host: h, target: "/api/push/key"})
		}
	}
	subs := "/api/rooms/" + room + "/push/subscriptions"
	sched := "/api/rooms/" + room + "/push/schedule"
	endpoint := "https://fcm.googleapis.com/fcm/send/parity-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	mustDo(t, w.hostthisd, request{method: "PUT", host: host, target: subs, body: []byte(subscription(endpoint))}, 204)
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	schedule := `{"tz":"America/New_York","items":[{"id":"morning","at":"08:00","days":[1,2,3],"title":"Rota","bodyKey":"push:{date}","url":"/","tag":"t"},` +
		`{"id":"once","when":"` + future + `","title":"Once","body":"Deep clean"}]}`
	mustDo(t, w.hostthisd, request{method: "PUT", host: host, target: sched, body: []byte(schedule)}, 204)

	w.check(t, get(host, subs))
	w.check(t, get(host, sched))
	for _, body := range []string{
		subscription(endpoint), subscription("https://example.com/push"), subscription("http://fcm.googleapis.com/x"),
		`{"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"AAAA","auth":"AAAA"}}`,
		`{"endpoint":""}`, `not json`, ``, `{"endpoint":"https://fcm.googleapis.com/x"} trailing`, `[1]`,
	} {
		w.check(t, request{method: "PUT", host: host, target: subs, body: []byte(body)})
	}
	for _, body := range []string{
		schedule, `{"tz":"","items":[]}`, `{"tz":"Not/AZone","items":[]}`,
		`{"tz":"UTC","items":[{"id":"bad id","at":"08:00","days":[1],"title":"t","body":"b"}]}`,
		`{"tz":"UTC","items":[{"id":"x","at":"25:00","days":[1],"title":"t","body":"b"}]}`,
		`{"tz":"UTC","items":[{"id":"x","when":"2001-01-01T00:00:00Z","title":"t","body":"b"}]}`,
		`{"tz":"UTC","items":[{"id":"x","at":"08:00","days":[1.5],"title":"t","body":"b"}]}`,
		`{"tz":"UTC","items":[{"id":"x","at":"08:00","days":[1],"title":"t","body":"b","bodyKey":"k"}]}`,
		`{"TZ":"UTC","items":null}`, `nope`,
	} {
		w.check(t, request{method: "PUT", host: host, target: sched, body: []byte(body)})
	}
	w.check(t, get(host, sched))
	for _, body := range []string{`{"endpoint":"https://fcm.googleapis.com/fcm/send/absent"}`, `{"endpoint":""}`, `bad`} {
		w.check(t, request{method: "DELETE", host: host, target: subs, body: []byte(body)})
	}
	for _, target := range []string{subs, sched, "/api/rooms/" + room + "/push/test", "/api/rooms/" + room + "/push/other", "/api/rooms/" + room + "/push"} {
		for _, m := range []string{"PATCH", "POST", "GET"} {
			if strings.HasSuffix(target, "/test") && m == "POST" {
				continue
			}
			w.check(t, request{method: m, host: host, target: target})
		}
	}
	for _, target := range []string{"/api/rooms/" + missing + "/push/subscriptions", "/api/rooms/" + missing + "/push/schedule"} {
		w.check(t, get(host, target))
		w.check(t, get(never, target))
	}

	// A test sends to every subscription, so it runs only in rooms that hold
	// none. Each server's first test in a room of its own is a 202; a second
	// within the minute is a 429 whose Retry-After counts down.
	var first []response
	tested := ""
	for _, s := range append([]*server{w.hostthisd}, w.workers...) {
		id := w.createRoom(t, w.hostthisd, host)
		first = append(first, mustDo(t, s, request{method: "POST", host: host, target: "/api/rooms/" + id + "/push/test"}, 202))
		tested = id
	}
	for i, res := range first[1:] {
		if diffs := differences(first[0], res); len(diffs) > 0 {
			t.Errorf("first push test through %s differs from hostthisd's:\n  %s", w.workers[i].name, strings.Join(diffs, "\n  "))
		}
	}
	again := request{method: "POST", host: host, target: "/api/rooms/" + tested + "/push/test"}
	h := mustDo(t, w.hostthisd, again, 429)
	hw, _ := strconv.Atoi(h.header.Get("Retry-After"))
	h.header.Del("Retry-After")
	for _, s := range w.workers {
		got := mustDo(t, s, again, 429)
		gw, _ := strconv.Atoi(got.header.Get("Retry-After"))
		if hw < 1 || gw < 1 || hw-gw > 1 || gw-hw > 1 {
			t.Errorf("second push test: Retry-After hostthisd %d, %s %d", hw, s.name, gw)
		}
		got.header.Del("Retry-After")
		if diffs := differences(h, got); len(diffs) > 0 {
			t.Errorf("second push test through %s differs from hostthisd's:\n  %s", s.name, strings.Join(diffs, "\n  "))
		}
	}
	w.expect(t, request{method: "POST", host: never, target: "/api/rooms/" + tested + "/push/test"}, 404)
	w.expect(t, request{method: "POST", host: host, target: "/api/rooms/" + missing + "/push/test"}, 404)
}
