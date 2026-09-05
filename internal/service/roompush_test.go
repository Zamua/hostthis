package service

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

func newPushSvc(t *testing.T) (*RoomPush, *Rooms, *fixedClock) {
	t.Helper()
	repo := storage.NewMemRoomRepo(storagetest.NewRepo(t))
	clk := &fixedClock{t: time.Now().UTC().Truncate(time.Second)}
	push := NewRoomPush(repo)
	push.Now = clk.now
	rooms := NewRooms(repo)
	rooms.Now = clk.now
	return push, rooms, clk
}

func testSub(n int) domain.PushSubscription {
	p := make([]byte, 65)
	p[0] = 0x04
	return domain.PushSubscription{
		Endpoint: fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", n),
		Keys: domain.PushKeys{
			P256DH: base64.RawURLEncoding.EncodeToString(p),
			Auth:   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		},
	}
}

func TestRoomPushSvc_SubscriptionsRoundTrip(t *testing.T) {
	push, rooms, _ := newPushSvc(t)
	room, err := rooms.Create("app12345", "10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := push.PutSubscription(room.AppSlug, room.ID, testSub(1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	subs, err := push.ListSubscriptions(room.AppSlug, room.ID)
	if err != nil || len(subs) != 1 || subs[0].Endpoint != testSub(1).Endpoint {
		t.Fatalf("list = %+v, %v", subs, err)
	}
	if err := push.DeleteSubscription(room.AppSlug, room.ID, testSub(1).Endpoint); err != nil {
		t.Fatalf("delete: %v", err)
	}
	subs, err = push.ListSubscriptions(room.AppSlug, room.ID)
	if err != nil || len(subs) != 0 || subs == nil {
		t.Fatalf("list after delete = %#v, %v (want empty non-nil)", subs, err)
	}
}

func TestRoomPushSvc_ValidationAndCapAndNotFound(t *testing.T) {
	push, rooms, clk := newPushSvc(t)
	room, _ := rooms.Create("app12345", "10.0.0.0/24")

	bad := testSub(1)
	bad.Endpoint = "http://plain"
	if err := push.PutSubscription(room.AppSlug, room.ID, bad); !errors.Is(err, domain.ErrPushInvalid) {
		t.Fatalf("invalid sub err = %v", err)
	}
	if err := push.DeleteSubscription(room.AppSlug, room.ID, ""); !errors.Is(err, domain.ErrPushInvalid) {
		t.Fatalf("empty endpoint delete err = %v", err)
	}
	if err := push.PutSchedule(room.AppSlug, room.ID, domain.PushSchedule{TZ: "Nowhere/Nope",
		Items: []domain.PushItem{{ID: "x", When: "2999-09-12T08:00:00Z", Title: "t", Body: "b"}}}); !errors.Is(err, domain.ErrPushInvalid) {
		t.Fatalf("invalid schedule err = %v", err)
	}
	// A one-shot is judged against the service clock: one second behind is
	// refused, one second ahead is stored.
	past := clk.now().Add(-time.Second).Format(time.RFC3339)
	if err := push.PutSchedule(room.AppSlug, room.ID, domain.PushSchedule{TZ: "UTC",
		Items: []domain.PushItem{{ID: "x", When: past, Title: "t", Body: "b"}}}); !errors.Is(err, domain.ErrPushInvalid) {
		t.Fatalf("past when err = %v", err)
	}
	future := clk.now().Add(time.Second).Format(time.RFC3339)
	if err := push.PutSchedule(room.AppSlug, room.ID, domain.PushSchedule{TZ: "UTC",
		Items: []domain.PushItem{{ID: "x", When: future, Title: "t", Body: "b"}}}); err != nil {
		t.Fatalf("future when: %v", err)
	}
	if err := push.PutSchedule(room.AppSlug, room.ID, domain.PushSchedule{Items: []domain.PushItem{}}); !errors.Is(err, domain.ErrPushInvalid) {
		t.Fatalf("clear without tz err = %v", err)
	}

	for i := range domain.MaxPushSubscriptions {
		if err := push.PutSubscription(room.AppSlug, room.ID, testSub(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := push.PutSubscription(room.AppSlug, room.ID, testSub(99)); !errors.Is(err, ErrPushCap) {
		t.Fatalf("cap err = %v, want ErrPushCap", err)
	}

	missing := domain.NewRoomID()
	if err := push.PutSubscription(room.AppSlug, missing, testSub(1)); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("missing room put err = %v", err)
	}
	if _, err := push.ListSubscriptions(room.AppSlug, missing); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("missing room list err = %v", err)
	}
	if _, err := push.GetSchedule(room.AppSlug, missing); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("missing room schedule err = %v", err)
	}
	if _, err := push.Test(room.AppSlug, missing); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("missing room test err = %v", err)
	}
}

func TestRoomPushSvc_ScheduleRoundTripAndEmptyItems(t *testing.T) {
	push, rooms, _ := newPushSvc(t)
	room, _ := rooms.Create("app12345", "10.0.0.0/24")
	got, err := push.GetSchedule(room.AppSlug, room.ID)
	if err != nil || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("unset schedule = %#v, %v (want empty non-nil items)", got, err)
	}
	want := domain.PushSchedule{TZ: "Europe/Paris", Items: []domain.PushItem{
		{ID: "a", At: "08:00", Days: []int{1}, Title: "t", BodyKey: "push:{date}"},
	}}
	if err := push.PutSchedule(room.AppSlug, room.ID, want); err != nil {
		t.Fatalf("put schedule: %v", err)
	}
	got, err = push.GetSchedule(room.AppSlug, room.ID)
	if err != nil || got.TZ != want.TZ || len(got.Items) != 1 || got.Items[0].ID != "a" {
		t.Fatalf("get schedule = %+v, %v", got, err)
	}
	if err := push.PutSchedule(room.AppSlug, room.ID, domain.PushSchedule{TZ: "Europe/Paris"}); err != nil {
		t.Fatalf("clear schedule: %v", err)
	}
	got, _ = push.GetSchedule(room.AppSlug, room.ID)
	if len(got.Items) != 0 {
		t.Fatalf("schedule not cleared: %+v", got)
	}
}

func TestRoomPushSvc_TestRateLimit(t *testing.T) {
	push, rooms, clk := newPushSvc(t)
	room, _ := rooms.Create("app12345", "10.0.0.0/24")
	if res, err := push.Test(room.AppSlug, room.ID); err != nil || res.Sent != 0 || res.Pruned != 0 {
		t.Fatalf("first test = %+v, %v", res, err)
	}
	clk.advance(10 * time.Second)
	_, err := push.Test(room.AppSlug, room.ID)
	rl, ok := errors.AsType[*PushRateLimit](err)
	if !ok || !errors.Is(err, ErrPushRateLimited) {
		t.Fatalf("second test err = %v, want PushRateLimit", err)
	}
	if rl.RetryAfter != 50*time.Second {
		t.Fatalf("RetryAfter = %v, want 50s", rl.RetryAfter)
	}
	clk.advance(50 * time.Second)
	if _, err := push.Test(room.AppSlug, room.ID); err != nil {
		t.Fatalf("test after interval: %v", err)
	}
}

func TestRoomPushSvc_KeyStablePerApp(t *testing.T) {
	push, _, _ := newPushSvc(t)
	k1, err := push.Key("app12345")
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := push.Key("app12345")
	other, _ := push.Key("app67890")
	if k1 != k2 || k1 == other {
		t.Fatalf("keys: %q %q %q", k1, k2, other)
	}
	raw, err := base64.RawURLEncoding.DecodeString(k1)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("key shape: len %d err %v", len(raw), err)
	}
}
