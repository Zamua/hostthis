package storage_test

// Room push in the backend-agnostic conformance suite: the storage contract
// for subscriptions, schedules, the app key and the test interval. Delivery is
// out of scope; the memory backend sends nothing and the celld backend has no
// subscriber to reach.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Zamua/hostthis/internal/domain"
)

func conformPushSub(n int) domain.PushSubscription {
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

func runRoomPushConformance(t *testing.T, name string, newRooms func(t *testing.T) roomConformanceStores) {
	t.Helper()
	t.Run(name+"/Rooms/Push/SubscriptionRoundTrip", func(t *testing.T) { conformPushSubscriptionRoundTrip(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/SubscriptionDedupeAndCap", func(t *testing.T) { conformPushSubscriptionDedupeAndCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/ScheduleRoundTrip", func(t *testing.T) { conformPushScheduleRoundTrip(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/CrossRoomIsolation", func(t *testing.T) { conformPushCrossRoomIsolation(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/NonexistentRoom", func(t *testing.T) { conformPushNonexistentRoom(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/KeyStablePerApp", func(t *testing.T) { conformPushKeyStablePerApp(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/TestInterval", func(t *testing.T) { conformPushTestInterval(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/ScheduleClear", func(t *testing.T) { conformPushScheduleClear(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/Push/Refusals", func(t *testing.T) { conformPushRefusals(t, newRooms(t).Rooms) })
}

// conformPushScheduleClear: a put of {tz, items: []} clears a set schedule
// and reads back as empty items.
func conformPushScheduleClear(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	set := domain.PushSchedule{TZ: "UTC", Items: []domain.PushItem{
		{ID: "x", At: "08:00", Days: []int{1}, Title: "t", Body: "b"},
	}}
	if err := rr.PutPushSchedule(room.AppSlug, room.ID, set, fixedNow); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := rr.PutPushSchedule(room.AppSlug, room.ID, domain.PushSchedule{TZ: "UTC", Items: []domain.PushItem{}}, fixedNow); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := rr.GetPushSchedule(room.AppSlug, room.ID)
	if err != nil || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("after clear = %#v, %v (want empty non-nil items)", got, err)
	}
}

// conformPushRefusals: both backends refuse the same documents and leave the
// room's push state untouched: a title of 64 runes but more than 64 bytes, a
// one-shot in the past, a schedule without tz, an endpoint off the push
// service allowlist.
func conformPushRefusals(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	wideTitle := strings.Repeat("\u00e9", domain.MaxPushTitleBytes)
	if utf8.RuneCountInString(wideTitle) != domain.MaxPushTitleBytes || len(wideTitle) <= domain.MaxPushTitleBytes {
		t.Fatal("fixture wrong")
	}
	for name, sched := range map[string]domain.PushSchedule{
		"title over 64 bytes": {TZ: "UTC", Items: []domain.PushItem{
			{ID: "x", At: "08:00", Days: []int{1}, Title: wideTitle, Body: "b"}}},
		"past when": {TZ: "UTC", Items: []domain.PushItem{
			{ID: "x", When: "2020-01-01T08:00:00Z", Title: "t", Body: "b"}}},
		"missing tz": {Items: []domain.PushItem{}},
	} {
		if err := rr.PutPushSchedule(room.AppSlug, room.ID, sched, fixedNow); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	got, err := rr.GetPushSchedule(room.AppSlug, room.ID)
	if err != nil || len(got.Items) != 0 {
		t.Fatalf("schedule after refusals = %+v, %v", got, err)
	}
	for _, endpoint := range []string{
		"https://push.example.test/send/1",
		"https://127.0.0.1/send/1",
		"https://localhost/send/1",
	} {
		sub := conformPushSub(1)
		sub.Endpoint = endpoint
		if err := rr.PutPushSubscription(room.AppSlug, room.ID, sub, fixedNow); err == nil {
			t.Errorf("%s: accepted", endpoint)
		}
	}
	subs, err := rr.ListPushSubscriptions(room.AppSlug, room.ID)
	if err != nil || len(subs) != 0 {
		t.Fatalf("subscriptions after refusals = %v, %v", subs, err)
	}
}

// conformPushSubscriptionRoundTrip: a put lists back with its endpoint and a
// non-zero added time, keys never listed; delete removes it and is idempotent.
func conformPushSubscriptionRoundTrip(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	subs, err := rr.ListPushSubscriptions(room.AppSlug, room.ID)
	if err != nil || len(subs) != 0 {
		t.Fatalf("fresh room list = %v, %v", subs, err)
	}
	if err := rr.PutPushSubscription(room.AppSlug, room.ID, conformPushSub(1), fixedNow); err != nil {
		t.Fatalf("put: %v", err)
	}
	subs, err = rr.ListPushSubscriptions(room.AppSlug, room.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("list = %v, %v", subs, err)
	}
	if subs[0].Endpoint != conformPushSub(1).Endpoint || subs[0].Added.IsZero() {
		t.Fatalf("listed = %+v", subs[0])
	}
	if err := rr.DeletePushSubscription(room.AppSlug, room.ID, conformPushSub(1).Endpoint); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := rr.DeletePushSubscription(room.AppSlug, room.ID, conformPushSub(1).Endpoint); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	subs, _ = rr.ListPushSubscriptions(room.AppSlug, room.ID)
	if len(subs) != 0 {
		t.Fatalf("list after delete = %v", subs)
	}
}

// conformPushSubscriptionDedupeAndCap: endpoints dedupe, the 17th distinct
// endpoint is ErrPushFull, a refresh at the cap still succeeds, and a delete
// frees a slot.
func conformPushSubscriptionDedupeAndCap(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	for range 3 {
		if err := rr.PutPushSubscription(room.AppSlug, room.ID, conformPushSub(0), fixedNow); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	subs, _ := rr.ListPushSubscriptions(room.AppSlug, room.ID)
	if len(subs) != 1 {
		t.Fatalf("dedupe: %d listed", len(subs))
	}
	for i := 1; i < domain.MaxPushSubscriptions; i++ {
		if err := rr.PutPushSubscription(room.AppSlug, room.ID, conformPushSub(i), fixedNow); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	err := rr.PutPushSubscription(room.AppSlug, room.ID, conformPushSub(99), fixedNow)
	if !errors.Is(err, domain.ErrPushFull) {
		t.Fatalf("over cap err = %v, want ErrPushFull", err)
	}
	if err := rr.PutPushSubscription(room.AppSlug, room.ID, conformPushSub(0), fixedNow); err != nil {
		t.Fatalf("refresh at cap: %v", err)
	}
	if err := rr.DeletePushSubscription(room.AppSlug, room.ID, conformPushSub(0).Endpoint); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := rr.PutPushSubscription(room.AppSlug, room.ID, conformPushSub(99), fixedNow); err != nil {
		t.Fatalf("put after delete: %v", err)
	}
	subs, _ = rr.ListPushSubscriptions(room.AppSlug, room.ID)
	if len(subs) != domain.MaxPushSubscriptions {
		t.Fatalf("listed %d, want %d", len(subs), domain.MaxPushSubscriptions)
	}
}

// conformPushScheduleRoundTrip: unset reads as empty items, a put reads back
// field for field, and an empty put clears.
func conformPushScheduleRoundTrip(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	got, err := rr.GetPushSchedule(room.AppSlug, room.ID)
	if err != nil || len(got.Items) != 0 {
		t.Fatalf("unset schedule = %+v, %v", got, err)
	}
	want := domain.PushSchedule{TZ: "America/New_York", Items: []domain.PushItem{
		{ID: "morning", At: "08:00", Days: []int{1, 2, 3, 4, 5, 6}, Title: "Rota", BodyKey: "push:{date}", URL: "/?room=x#/", Tag: "rota-today"},
		{ID: "once", When: "2026-09-12T08:00:00-04:00", Title: "Rota", Body: "Deep clean"},
	}}
	if err := rr.PutPushSchedule(room.AppSlug, room.ID, want, fixedNow); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err = rr.GetPushSchedule(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TZ != want.TZ || len(got.Items) != 2 {
		t.Fatalf("got = %+v", got)
	}
	for i := range want.Items {
		w, g := want.Items[i], got.Items[i]
		if g.ID != w.ID || g.At != w.At || g.When != w.When || g.Title != w.Title || g.Body != w.Body ||
			g.BodyKey != w.BodyKey || g.URL != w.URL || g.Tag != w.Tag || len(g.Days) != len(w.Days) {
			t.Fatalf("item %d = %+v, want %+v", i, g, w)
		}
	}
	if err := rr.PutPushSchedule(room.AppSlug, room.ID, domain.PushSchedule{TZ: "UTC", Items: []domain.PushItem{}}, fixedNow); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = rr.GetPushSchedule(room.AppSlug, room.ID)
	if len(got.Items) != 0 {
		t.Fatalf("not cleared: %+v", got)
	}
}

// conformPushCrossRoomIsolation: subscriptions and schedules are per room.
func conformPushCrossRoomIsolation(t *testing.T, rr conformanceRoomRepo) {
	a := mkConformRoom(t, rr, "app12345", fixedNow)
	b := mkConformRoom(t, rr, "app12345", fixedNow)
	if err := rr.PutPushSubscription(a.AppSlug, a.ID, conformPushSub(1), fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := rr.PutPushSchedule(a.AppSlug, a.ID, domain.PushSchedule{TZ: "UTC",
		Items: []domain.PushItem{{ID: "x", When: "2026-09-12T08:00:00Z", Title: "t", Body: "b"}}}, fixedNow); err != nil {
		t.Fatal(err)
	}
	subs, _ := rr.ListPushSubscriptions(b.AppSlug, b.ID)
	sched, _ := rr.GetPushSchedule(b.AppSlug, b.ID)
	if len(subs) != 0 || len(sched.Items) != 0 {
		t.Fatalf("room b sees room a's push state: %v %+v", subs, sched)
	}
}

// conformPushNonexistentRoom: every room-scoped op on a missing room is
// ErrNotFound.
func conformPushNonexistentRoom(t *testing.T, rr conformanceRoomRepo) {
	const app = domain.Slug("app12345")
	id := domain.NewRoomID()
	if err := rr.PutPushSubscription(app, id, conformPushSub(1), fixedNow); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("put sub err = %v", err)
	}
	if err := rr.DeletePushSubscription(app, id, conformPushSub(1).Endpoint); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("delete sub err = %v", err)
	}
	if _, err := rr.ListPushSubscriptions(app, id); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("list err = %v", err)
	}
	if err := rr.PutPushSchedule(app, id, domain.PushSchedule{TZ: "UTC"}, fixedNow); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("put schedule err = %v", err)
	}
	if _, err := rr.GetPushSchedule(app, id); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get schedule err = %v", err)
	}
	if _, err := rr.TestPush(app, id, fixedNow); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("test err = %v", err)
	}
}

// conformPushKeyStablePerApp: one key per app, a 65-byte uncompressed point,
// different across apps.
func conformPushKeyStablePerApp(t *testing.T, rr conformanceRoomRepo) {
	k1, err := rr.PushKey("app12345")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	k2, err := rr.PushKey("app12345")
	if err != nil || k1 != k2 {
		t.Fatalf("key not stable: %q %q %v", k1, k2, err)
	}
	other, err := rr.PushKey("app67890")
	if err != nil || other == k1 {
		t.Fatalf("key shared across apps: %q %v", other, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(k1)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("key %q: %d bytes, err %v", k1, len(raw), err)
	}
}

// conformPushTestInterval: a room with no subscribers reports zero sends, and
// a second test inside the interval is rate limited with a positive wait.
func conformPushTestInterval(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	res, err := rr.TestPush(room.AppSlug, room.ID, fixedNow)
	if err != nil || res.Sent != 0 || res.Pruned != 0 {
		t.Fatalf("first test = %+v, %v", res, err)
	}
	_, err = rr.TestPush(room.AppSlug, room.ID, fixedNow.Add(time.Second))
	rl, ok := errors.AsType[*domain.PushRateLimit](err)
	if !ok || !errors.Is(err, domain.ErrPushRateLimited) {
		t.Fatalf("second test err = %v, want PushRateLimit", err)
	}
	if rl.RetryAfter <= 0 || rl.RetryAfter > domain.PushTestInterval {
		t.Fatalf("RetryAfter = %v", rl.RetryAfter)
	}
}
