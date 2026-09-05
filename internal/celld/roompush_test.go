package celld

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

const (
	pushTestApp  = domain.Slug("abcdefgh")
	pushTestID   = domain.RoomID("123e4567-e89b-42d3-a456-426614174000")
	pushTestRoom = "abcdefgh|123e4567-e89b-42d3-a456-426614174000"
)

func fakeRooms(status int, body string) (*fakeCell, *RoomRepo) {
	f := fixedCell(status, body)
	return f, NewRoomRepo("https://cell", f.client())
}

func TestRoomPushCelld_PushKey(t *testing.T) {
	f, repo := fakeRooms(http.StatusOK, `{"key":"BAAA"}`)
	key, err := repo.PushKey(pushTestApp)
	if err != nil || key != "BAAA" {
		t.Fatalf("key = %q, %v", key, err)
	}
	if req := f.last(); req.Method != http.MethodPost || req.Path != "/paste/pushkey" || req.Query != "slug=abcdefgh" {
		t.Fatalf("request = %s %s?%s", req.Method, req.Path, req.Query)
	}
	_, repo = fakeRooms(http.StatusNotFound, "not found\n")
	if _, err := repo.PushKey(pushTestApp); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
}

func TestRoomPushCelld_SubscriptionPutSendsBrowserJSON(t *testing.T) {
	f, repo := fakeRooms(http.StatusNoContent, "")
	repo.PushSubject = "https://hostthis.test"
	sub := domain.PushSubscription{Endpoint: "https://p/1", Keys: domain.PushKeys{P256DH: "BA", Auth: "AA"}}
	if err := repo.PutPushSubscription(pushTestApp, pushTestID, sub, time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	req := f.last()
	if req.Method != http.MethodPost || req.Path != "/room/pushsubput" || req.Query != "room="+urlQuery(pushTestRoom) {
		t.Fatalf("request = %s %s?%s", req.Method, req.Path, req.Query)
	}
	got := req.fields(t)
	keys, _ := got["keys"].(map[string]any)
	if got["endpoint"] != "https://p/1" || keys["p256dh"] != "BA" || keys["auth"] != "AA" ||
		got["subject"] != "https://hostthis.test" || got["now"] != float64(5) {
		t.Fatalf("body = %s", req.Body)
	}

	_, repo = fakeRooms(http.StatusRequestEntityTooLarge, `{"error":"too-many-subscriptions"}`)
	if err := repo.PutPushSubscription(pushTestApp, pushTestID, sub, time.Now()); !errors.Is(err, domain.ErrPushFull) {
		t.Fatalf("413 err = %v", err)
	}
	// Only the subscription cap is a 413 the caller can act on; any other
	// refusal is a contract fault, since the domain validated first.
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusRequestEntityTooLarge, ``},
		{http.StatusBadRequest, `{"error":"invalid-endpoint"}`},
		{http.StatusBadRequest, `{"error":"invalid-keys"}`},
	} {
		_, repo = fakeRooms(tc.status, tc.body)
		err := repo.PutPushSubscription(pushTestApp, pushTestID, sub, time.Now())
		if err == nil || errors.Is(err, domain.ErrPushFull) || errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%d %s: err = %v, want a plain error", tc.status, tc.body, err)
		}
	}
	_, repo = fakeRooms(http.StatusNotFound, "")
	if err := repo.PutPushSubscription(pushTestApp, pushTestID, sub, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
}

func TestRoomPushCelld_SubscriptionDeleteAndList(t *testing.T) {
	f, repo := fakeRooms(http.StatusNoContent, "")
	if err := repo.DeletePushSubscription(pushTestApp, pushTestID, "https://p/1"); err != nil {
		t.Fatal(err)
	}
	if req := f.last(); req.Path != "/room/pushsubdel" || req.fields(t)["endpoint"] != "https://p/1" {
		t.Fatalf("request = %s body %s", req.Path, req.Body)
	}

	f, repo = fakeRooms(http.StatusOK,
		`{"subscriptions":[{"endpoint":"https://p/1","added":"2026-09-05T10:00:00Z"},{"endpoint":"https://p/2","added":1788602400000}]}`)
	subs, err := repo.ListPushSubscriptions(pushTestApp, pushTestID)
	if err != nil {
		t.Fatal(err)
	}
	if req := f.last(); req.Method != http.MethodPost || req.Path != "/room/pushsublist" || len(req.Body) != 0 {
		t.Fatalf("request = %s %s body %q", req.Method, req.Path, req.Body)
	}
	want := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if len(subs) != 2 || subs[0].Endpoint != "https://p/1" || !subs[0].Added.Equal(want) || !subs[1].Added.Equal(want) {
		t.Fatalf("subs = %+v", subs)
	}
}

func TestRoomPushCelld_Schedule(t *testing.T) {
	f, repo := fakeRooms(http.StatusNoContent, "")
	repo.PushSubject = "https://hostthis.test"
	sched := domain.PushSchedule{TZ: "UTC", Items: []domain.PushItem{{ID: "a", At: "08:00", Days: []int{1}, Title: "t", Body: "b"}}}
	if err := repo.PutPushSchedule(pushTestApp, pushTestID, sched, time.UnixMilli(9)); err != nil {
		t.Fatal(err)
	}
	if f.last().Path != "/room/pushscheduleput" {
		t.Fatalf("path = %s", f.last().Path)
	}
	var sent struct {
		domain.PushSchedule
		Subject string `json:"subject"`
		Now     int64  `json:"now"`
	}
	if err := json.Unmarshal(f.last().Body, &sent); err != nil || sent.TZ != "UTC" || len(sent.Items) != 1 ||
		sent.Items[0].At != "08:00" || sent.Subject != "https://hostthis.test" || sent.Now != 9 {
		t.Fatalf("sent = %s (%v)", f.last().Body, err)
	}
	// The item cap is the one 413 the caller can act on; any other 413 or a
	// 400 is a contract fault, since the domain validated first.
	_, r413 := fakeRooms(http.StatusRequestEntityTooLarge, `{"error":"too-many-items"}`)
	if err := r413.PutPushSchedule(pushTestApp, pushTestID, sched, time.Now()); !errors.Is(err, domain.ErrPushFull) {
		t.Fatalf("413 too-many-items err = %v, want ErrPushFull", err)
	}
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusRequestEntityTooLarge, ``},
		{http.StatusBadRequest, `{"error":"invalid-tz"}`},
		{http.StatusBadRequest, `{"error":"invalid-when"}`},
	} {
		_, r := fakeRooms(tc.status, tc.body)
		err := r.PutPushSchedule(pushTestApp, pushTestID, sched, time.Now())
		if err == nil || errors.Is(err, domain.ErrPushFull) || errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%d %s: err = %v, want a plain error", tc.status, tc.body, err)
		}
	}
	_, r404 := fakeRooms(http.StatusNotFound, "not found\n")
	if err := r404.PutPushSchedule(pushTestApp, pushTestID, sched, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
	// A cleared schedule sends an empty array, never null.
	if err := repo.PutPushSchedule(pushTestApp, pushTestID, domain.PushSchedule{TZ: "UTC"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(f.last().Body, &raw)
	if string(raw["items"]) != "[]" {
		t.Fatalf("cleared items = %s", raw["items"])
	}

	f, repo = fakeRooms(http.StatusOK, `{"tz":"Europe/Paris","items":[{"id":"x","when":"2026-09-12T08:00:00Z","title":"t","body":"b"}]}`)
	got, err := repo.GetPushSchedule(pushTestApp, pushTestID)
	if err != nil || f.last().Method != http.MethodPost || f.last().Path != "/room/pushscheduleget" {
		t.Fatalf("get: %v %s %s", err, f.last().Method, f.last().Path)
	}
	if got.TZ != "Europe/Paris" || len(got.Items) != 1 || got.Items[0].When != "2026-09-12T08:00:00Z" {
		t.Fatalf("got = %+v", got)
	}
	_, repo = fakeRooms(http.StatusOK, `{"tz":"","items":null}`)
	got, _ = repo.GetPushSchedule(pushTestApp, pushTestID)
	if got.Items == nil {
		t.Fatal("null items not normalized to empty")
	}
}

func TestRoomPushCelld_Test(t *testing.T) {
	f, repo := fakeRooms(http.StatusOK, `{"sent":3,"pruned":1}`)
	res, err := repo.TestPush(pushTestApp, pushTestID, time.UnixMilli(7))
	if err != nil || res.Sent != 3 || res.Pruned != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	req := f.last()
	if req.Method != http.MethodPost || req.Path != "/room/pushtest" || req.Query != "room="+urlQuery(pushTestRoom) ||
		req.fields(t)["now"] != float64(7) {
		t.Fatalf("request = %s %s?%s body %s", req.Method, req.Path, req.Query, req.Body)
	}

	_, repo = fakeRooms(http.StatusTooManyRequests, `{"error":"rate-limited","retryAfter":42}`)
	_, err = repo.TestPush(pushTestApp, pushTestID, time.Now())
	rl, ok := errors.AsType[*domain.PushRateLimit](err)
	if !ok || rl.RetryAfter != 42*time.Second {
		t.Fatalf("429 err = %v", err)
	}
	_, repo = fakeRooms(http.StatusNotFound, "not found\n")
	if _, err := repo.TestPush(pushTestApp, pushTestID, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
	_, repo = fakeRooms(http.StatusInternalServerError, "boom")
	if _, err := repo.TestPush(pushTestApp, pushTestID, time.Now()); err == nil {
		t.Fatal("500 not an error")
	}
}
