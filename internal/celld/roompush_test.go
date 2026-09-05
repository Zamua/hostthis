package celld

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

const pushTestRoom = "abcdefgh|123e4567-e89b-42d3-a456-426614174000"

// fakeCell records the last request and answers with the configured status
// and body.
type fakeCell struct {
	t      *testing.T
	status int
	body   string
	path   string
	query  string
	method string
	got    []byte
}

func (f *fakeCell) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.path, f.query, f.method = r.URL.Path, r.URL.RawQuery, r.Method
	var err error
	if f.got, err = readAll(r); err != nil {
		f.t.Fatalf("read body: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

func readAll(r *http.Request) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

func newFake(t *testing.T, status int, body string) (*fakeCell, *RoomRepo) {
	f := &fakeCell{t: t, status: status, body: body}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewRoomRepo(srv.URL, srv.Client())
}

func TestRoomPushCelld_PushKey(t *testing.T) {
	f, repo := newFake(t, http.StatusOK, `{"key":"BAAA"}`)
	key, err := repo.PushKey("abcdefgh")
	if err != nil || key != "BAAA" {
		t.Fatalf("key = %q, %v", key, err)
	}
	if f.method != http.MethodPost || f.path != "/paste/pushkey" || f.query != "slug=abcdefgh" {
		t.Fatalf("request = %s %s?%s", f.method, f.path, f.query)
	}
	_, repo = newFake(t, http.StatusNotFound, "not found\n")
	if _, err := repo.PushKey("abcdefgh"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
}

func TestRoomPushCelld_SubscriptionPutSendsBrowserJSON(t *testing.T) {
	f, repo := newFake(t, http.StatusNoContent, "")
	repo.PushSubject = "https://hostthis.test"
	sub := domain.PushSubscription{Endpoint: "https://p/1", Keys: domain.PushKeys{P256DH: "BA", Auth: "AA"}}
	if err := repo.PutPushSubscription("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sub, time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	if f.method != http.MethodPost || f.path != "/room/pushsubput" || f.query != "room="+queryEscaped(pushTestRoom) {
		t.Fatalf("request = %s %s?%s", f.method, f.path, f.query)
	}
	var got map[string]any
	if err := json.Unmarshal(f.got, &got); err != nil {
		t.Fatal(err)
	}
	keys, _ := got["keys"].(map[string]any)
	if got["endpoint"] != "https://p/1" || keys["p256dh"] != "BA" || keys["auth"] != "AA" ||
		got["subject"] != "https://hostthis.test" || got["now"] != float64(5) {
		t.Fatalf("body = %s", f.got)
	}

	_, repo = newFake(t, http.StatusRequestEntityTooLarge, `{"error":"too-many-subscriptions"}`)
	if err := repo.PutPushSubscription("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sub, time.Now()); !errors.Is(err, domain.ErrPushFull) {
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
		_, repo = newFake(t, tc.status, tc.body)
		err := repo.PutPushSubscription("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sub, time.Now())
		if err == nil || errors.Is(err, domain.ErrPushFull) || errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%d %s: err = %v, want a plain error", tc.status, tc.body, err)
		}
	}
	_, repo = newFake(t, http.StatusNotFound, "")
	if err := repo.PutPushSubscription("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sub, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
}

func queryEscaped(s string) string { return urlQuery(s) }

func TestRoomPushCelld_SubscriptionDeleteAndList(t *testing.T) {
	f, repo := newFake(t, http.StatusNoContent, "")
	if err := repo.DeletePushSubscription("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", "https://p/1"); err != nil {
		t.Fatal(err)
	}
	if f.path != "/room/pushsubdel" || string(f.got) != `{"endpoint":"https://p/1"}` {
		t.Fatalf("request = %s body %s", f.path, f.got)
	}

	f, repo = newFake(t, http.StatusOK,
		`{"subscriptions":[{"endpoint":"https://p/1","added":"2026-09-05T10:00:00Z"},{"endpoint":"https://p/2","added":1788602400000}]}`)
	subs, err := repo.ListPushSubscriptions("abcdefgh", "123e4567-e89b-42d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	if f.method != http.MethodPost || f.path != "/room/pushsublist" || len(f.got) != 0 {
		t.Fatalf("request = %s %s body %q", f.method, f.path, f.got)
	}
	want := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if len(subs) != 2 || subs[0].Endpoint != "https://p/1" || !subs[0].Added.Equal(want) || !subs[1].Added.Equal(want) {
		t.Fatalf("subs = %+v", subs)
	}
}

func TestRoomPushCelld_Schedule(t *testing.T) {
	f, repo := newFake(t, http.StatusNoContent, "")
	repo.PushSubject = "https://hostthis.test"
	sched := domain.PushSchedule{TZ: "UTC", Items: []domain.PushItem{{ID: "a", At: "08:00", Days: []int{1}, Title: "t", Body: "b"}}}
	if err := repo.PutPushSchedule("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sched, time.UnixMilli(9)); err != nil {
		t.Fatal(err)
	}
	if f.path != "/room/pushscheduleput" {
		t.Fatalf("path = %s", f.path)
	}
	var sent struct {
		domain.PushSchedule
		Subject string `json:"subject"`
		Now     int64  `json:"now"`
	}
	if err := json.Unmarshal(f.got, &sent); err != nil || sent.TZ != "UTC" || len(sent.Items) != 1 ||
		sent.Items[0].At != "08:00" || sent.Subject != "https://hostthis.test" || sent.Now != 9 {
		t.Fatalf("sent = %s (%v)", f.got, err)
	}
	// The item cap is the one 413 the caller can act on; any other 413 or a
	// 400 is a contract fault, since the domain validated first.
	_, r413 := newFake(t, http.StatusRequestEntityTooLarge, `{"error":"too-many-items"}`)
	if err := r413.PutPushSchedule("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sched, time.Now()); !errors.Is(err, domain.ErrPushFull) {
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
		_, r := newFake(t, tc.status, tc.body)
		err := r.PutPushSchedule("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sched, time.Now())
		if err == nil || errors.Is(err, domain.ErrPushFull) || errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%d %s: err = %v, want a plain error", tc.status, tc.body, err)
		}
	}
	_, r404 := newFake(t, http.StatusNotFound, "not found\n")
	if err := r404.PutPushSchedule("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", sched, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
	// A cleared schedule sends an empty array, never null.
	if err := repo.PutPushSchedule("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", domain.PushSchedule{TZ: "UTC"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(f.got, &raw)
	if string(raw["items"]) != "[]" {
		t.Fatalf("cleared items = %s", raw["items"])
	}

	f, repo = newFake(t, http.StatusOK, `{"tz":"Europe/Paris","items":[{"id":"x","when":"2026-09-12T08:00:00Z","title":"t","body":"b"}]}`)
	got, err := repo.GetPushSchedule("abcdefgh", "123e4567-e89b-42d3-a456-426614174000")
	if err != nil || f.method != http.MethodPost || f.path != "/room/pushscheduleget" {
		t.Fatalf("get: %v %s %s", err, f.method, f.path)
	}
	if got.TZ != "Europe/Paris" || len(got.Items) != 1 || got.Items[0].When != "2026-09-12T08:00:00Z" {
		t.Fatalf("got = %+v", got)
	}
	_, repo = newFake(t, http.StatusOK, `{"tz":"","items":null}`)
	got, _ = repo.GetPushSchedule("abcdefgh", "123e4567-e89b-42d3-a456-426614174000")
	if got.Items == nil {
		t.Fatal("null items not normalized to empty")
	}
}

func TestRoomPushCelld_Test(t *testing.T) {
	f, repo := newFake(t, http.StatusOK, `{"sent":3,"pruned":1}`)
	res, err := repo.TestPush("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", time.UnixMilli(7))
	if err != nil || res.Sent != 3 || res.Pruned != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if f.method != http.MethodPost || f.path != "/room/pushtest" || f.query != "room="+queryEscaped(pushTestRoom) ||
		string(f.got) != `{"now":7}` {
		t.Fatalf("request = %s %s?%s body %s", f.method, f.path, f.query, f.got)
	}

	_, repo = newFake(t, http.StatusTooManyRequests, `{"error":"rate-limited","retryAfter":42}`)
	_, err = repo.TestPush("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", time.Now())
	rl, ok := errors.AsType[*domain.PushRateLimit](err)
	if !ok || rl.RetryAfter != 42*time.Second {
		t.Fatalf("429 err = %v", err)
	}
	_, repo = newFake(t, http.StatusNotFound, "not found\n")
	if _, err := repo.TestPush("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("404 err = %v", err)
	}
	_, repo = newFake(t, http.StatusInternalServerError, "boom")
	if _, err := repo.TestPush("abcdefgh", "123e4567-e89b-42d3-a456-426614174000", time.Now()); err == nil {
		t.Fatal("500 not an error")
	}
}
