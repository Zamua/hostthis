package domain

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func validSub() PushSubscription {
	p := make([]byte, 65)
	p[0] = 0x04
	return PushSubscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/abc",
		Keys:     PushKeys{P256DH: b64(p), Auth: b64(make([]byte, 16))},
	}
}

func TestValidatePushSubscription(t *testing.T) {
	if err := ValidatePushSubscription(validSub()); err != nil {
		t.Fatalf("valid subscription refused: %v", err)
	}
	// Padded base64url is what some encoders emit for the same bytes.
	padded := validSub()
	padded.Keys.Auth = base64.URLEncoding.EncodeToString(make([]byte, 16))
	if err := ValidatePushSubscription(padded); err != nil {
		t.Fatalf("padded base64url refused: %v", err)
	}

	cases := map[string]func(*PushSubscription){
		"empty endpoint":     func(s *PushSubscription) { s.Endpoint = "" },
		"http endpoint":      func(s *PushSubscription) { s.Endpoint = "http://fcm.googleapis.com/fcm/send/x" },
		"not a url":          func(s *PushSubscription) { s.Endpoint = "https://" },
		"endpoint too long":  func(s *PushSubscription) { s.Endpoint = "https://fcm.googleapis.com/" + strings.Repeat("a", 1024) },
		"p256dh not base64":  func(s *PushSubscription) { s.Keys.P256DH = "!!!" },
		"p256dh wrong len":   func(s *PushSubscription) { s.Keys.P256DH = b64(make([]byte, 64)) },
		"p256dh compressed":  func(s *PushSubscription) { s.Keys.P256DH = b64(make([]byte, 65)) },
		"auth wrong len":     func(s *PushSubscription) { s.Keys.Auth = b64(make([]byte, 15)) },
		"auth missing":       func(s *PushSubscription) { s.Keys.Auth = "" },
		"p256dh std base64":  func(s *PushSubscription) { s.Keys.P256DH = strings.ReplaceAll(s.Keys.P256DH, "A", "+") + "/" },
		"endpoint with ctrl": func(s *PushSubscription) { s.Endpoint = "https://fcm.googleapis.com/\x00" },
	}
	for name, mutate := range cases {
		s := validSub()
		mutate(&s)
		if err := ValidatePushSubscription(s); !errors.Is(err, ErrPushInvalid) {
			t.Errorf("%s: err = %v, want ErrPushInvalid", name, err)
		}
	}
}

func TestPushEndpointAtCapIsAccepted(t *testing.T) {
	s := validSub()
	const base = "https://fcm.googleapis.com/"
	s.Endpoint = base + strings.Repeat("a", MaxPushEndpointBytes-len(base))
	if len(s.Endpoint) != MaxPushEndpointBytes {
		t.Fatal("fixture wrong")
	}
	if err := ValidatePushSubscription(s); err != nil {
		t.Fatalf("endpoint of exactly %d bytes refused: %v", MaxPushEndpointBytes, err)
	}
}

func TestPushEndpointHostAllowlist(t *testing.T) {
	allowed := []string{
		"https://web.push.apple.com/QAbc",
		"https://api.push.apple.com/x",
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://region.push.services.mozilla.com/x",
		"https://db5p.notify.windows.com/w/?token=x",
		"https://FCM.googleapis.com/fcm/send/abc",
		"https://fcm.googleapis.com:443/fcm/send/abc",
		"https://push.apple.com/x",
		"https://push.services.mozilla.com/x",
		"https://notify.windows.com/x",
		"https://sub.fcm.googleapis.com/x",
	}
	for _, ep := range allowed {
		s := validSub()
		s.Endpoint = ep
		if err := ValidatePushSubscription(s); err != nil {
			t.Errorf("%s refused: %v", ep, err)
		}
	}
	refused := []string{
		"https://push.example.test/send/abc",
		"https://example.com/",
		"https://localhost/x",
		"https://127.0.0.1/x",
		"https://[::1]/x",
		"https://10.0.0.5/x",
		"https://notpush.apple.com/x",
		"https://notfcm.googleapis.com/x",
		"https://fcm.googleapis.com.evil.example/x",
		"https://xnotify.windows.com/x",
		"https://googleapis.com/x",
		"https://fcm.googleapis.com@evil.test/x",
		"https://evil.test#fcm.googleapis.com",
	}
	for _, ep := range refused {
		s := validSub()
		s.Endpoint = ep
		if err := ValidatePushSubscription(s); !errors.Is(err, ErrPushInvalid) {
			t.Errorf("%s: err = %v, want ErrPushInvalid", ep, err)
		}
	}
}

// testNow is the reference clock for schedule validation.
var testNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func recurring(id string) PushItem {
	return PushItem{ID: id, At: "08:00", Days: []int{1, 2, 3}, Title: "Rota", BodyKey: "push:{date}"}
}

func oneShot(id string) PushItem {
	return PushItem{ID: id, When: "2026-09-12T08:00:00-04:00", Title: "Rota", Body: "Deep clean"}
}

func validSchedule() PushSchedule {
	return PushSchedule{TZ: "America/New_York", Items: []PushItem{recurring("morning"), oneShot("once")}}
}

func TestValidatePushSchedule_Valid(t *testing.T) {
	many := PushSchedule{TZ: "UTC"}
	for i := range MaxPushItems {
		many.Items = append(many.Items, oneShot("i"+string(rune('a'+i))))
	}
	cases := map[string]PushSchedule{
		"typical":                               validSchedule(),
		"empty items clears, tz still required": {TZ: "UTC"},
		"empty non-nil items":                   {TZ: "Europe/Paris", Items: []PushItem{}},
		"every field at its cap": {TZ: "UTC", Items: []PushItem{{
			ID: strings.Repeat("a", MaxPushIDLen), At: "23:59", Days: []int{0, 1, 2, 3, 4, 5, 6},
			Title: strings.Repeat("t", MaxPushTitleBytes), Body: strings.Repeat("b", MaxPushBodyBytes),
			URL: strings.Repeat("u", MaxPushURLBytes), Tag: strings.Repeat("g", MaxPushTagBytes),
		}}},
		"Z is an RFC 3339 offset": {TZ: "UTC", Items: []PushItem{{ID: "z", When: "2026-09-12T08:00:00Z", Title: "t", Body: "b"}}},
		"text caps count bytes, not runes": {TZ: "UTC", Items: []PushItem{{ID: "w", When: "2026-09-12T08:00:00Z",
			Title: strings.Repeat("é", MaxPushTitleBytes/2), Body: "b"}}},
		"one-shot one second ahead": {TZ: "UTC", Items: []PushItem{{ID: "s",
			When: testNow.Add(time.Second).Format(time.RFC3339), Title: "t", Body: "b"}}},
		"max item count": many,
	}
	for name, s := range cases {
		if err := ValidatePushSchedule(s, testNow); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestValidatePushSchedule_Invalid(t *testing.T) {
	cases := map[string]func(*PushSchedule){
		"missing tz":             func(s *PushSchedule) { s.TZ = "" },
		"missing tz empty items": func(s *PushSchedule) { s.TZ = ""; s.Items = nil },
		"Local tz empty items":   func(s *PushSchedule) { s.TZ = "Local"; s.Items = []PushItem{} },
		"title over 64 bytes":    func(s *PushSchedule) { s.Items[0].Title = strings.Repeat("é", MaxPushTitleBytes) },
		"url over 512 bytes":     func(s *PushSchedule) { s.Items[0].URL = strings.Repeat("é", MaxPushURLBytes/2+1) },
		"tag over 64 bytes":      func(s *PushSchedule) { s.Items[0].Tag = strings.Repeat("é", MaxPushTagBytes/2+1) },
		"when in the past":       func(s *PushSchedule) { s.Items[1].When = "2026-09-01T08:00:00Z" },
		"when equal to now":      func(s *PushSchedule) { s.Items[1].When = testNow.Format(time.RFC3339) },
		"unknown tz":             func(s *PushSchedule) { s.TZ = "Mars/Olympus" },
		"Local tz":               func(s *PushSchedule) { s.TZ = "Local" },
		"too many items": func(s *PushSchedule) {
			s.Items = make([]PushItem, MaxPushItems+1)
			for i := range s.Items {
				s.Items[i] = oneShot("i" + string(rune('a'+i)))
			}
		},
		"empty id":          func(s *PushSchedule) { s.Items[0].ID = "" },
		"id too long":       func(s *PushSchedule) { s.Items[0].ID = strings.Repeat("a", MaxPushIDLen+1) },
		"id bad charset":    func(s *PushSchedule) { s.Items[0].ID = "morn ing" },
		"duplicate id":      func(s *PushSchedule) { s.Items[1].ID = s.Items[0].ID },
		"both body kinds":   func(s *PushSchedule) { s.Items[0].Body = "x" },
		"neither body kind": func(s *PushSchedule) { s.Items[0].BodyKey = "" },
		"body too long":     func(s *PushSchedule) { s.Items[1].Body = strings.Repeat("b", MaxPushBodyBytes+1) },
		"bodyKey too long":  func(s *PushSchedule) { s.Items[0].BodyKey = strings.Repeat("k", MaxRoomKeyLen-5) + "{date}" },
		"bodyKey reserved":  func(s *PushSchedule) { s.Items[0].BodyKey = "push/{date}" },
		"bodyKey ws":        func(s *PushSchedule) { s.Items[0].BodyKey = "ws" },
		"empty title":       func(s *PushSchedule) { s.Items[0].Title = "" },
		"title too long":    func(s *PushSchedule) { s.Items[0].Title = strings.Repeat("t", MaxPushTitleBytes+1) },
		"url too long":      func(s *PushSchedule) { s.Items[0].URL = strings.Repeat("u", MaxPushURLBytes+1) },
		"tag too long":      func(s *PushSchedule) { s.Items[0].Tag = strings.Repeat("g", MaxPushTagBytes+1) },
		"at bad hour":       func(s *PushSchedule) { s.Items[0].At = "24:00" },
		"at bad minute":     func(s *PushSchedule) { s.Items[0].At = "08:60" },
		"at bad shape":      func(s *PushSchedule) { s.Items[0].At = "8:00" },
		"at with seconds":   func(s *PushSchedule) { s.Items[0].At = "08:00:00" },
		"days empty":        func(s *PushSchedule) { s.Items[0].Days = nil },
		"days out of range": func(s *PushSchedule) { s.Items[0].Days = []int{7} },
		"days negative":     func(s *PushSchedule) { s.Items[0].Days = []int{-1} },
		"days duplicate":    func(s *PushSchedule) { s.Items[0].Days = []int{1, 1} },
		"at without days":   func(s *PushSchedule) { s.Items[1].At = "08:00" },
		"days without at":   func(s *PushSchedule) { s.Items[1].Days = []int{1} },
		"when and at":       func(s *PushSchedule) { s.Items[0].When = "2026-09-12T08:00:00-04:00" },
		"when no offset":    func(s *PushSchedule) { s.Items[1].When = "2026-09-12T08:00:00" },
		"when not rfc3339":  func(s *PushSchedule) { s.Items[1].When = "September 12" },
		"nothing scheduled": func(s *PushSchedule) { s.Items[1].When = "" },
	}
	for name, mutate := range cases {
		s := validSchedule()
		mutate(&s)
		if err := ValidatePushSchedule(s, testNow); !errors.Is(err, ErrPushInvalid) {
			t.Errorf("%s: err = %v, want ErrPushInvalid", name, err)
		}
	}
}

func TestPushItemIsRecurringAndWhenTime(t *testing.T) {
	if !recurring("r").Recurring() || oneShot("o").Recurring() {
		t.Fatal("Recurring misreports item shape")
	}
	when, err := oneShot("o").WhenTime()
	if err != nil {
		t.Fatalf("WhenTime: %v", err)
	}
	want := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if !when.Equal(want) {
		t.Fatalf("WhenTime = %v, want %v", when, want)
	}
}

func TestValidateRoomKey_ReservedKeys(t *testing.T) {
	for _, key := range []string{"ws", "push", "push/", "push/subscriptions", "push/x/y"} {
		if err := ValidateRoomKey(key); !errors.Is(err, ErrRoomKeyReserved) {
			t.Errorf("%q: err = %v, want ErrRoomKeyReserved", key, err)
		}
	}
	for _, key := range []string{"push:2026-09-06", "pushy", "wsx", "ws/", "x/push", "PUSH"} {
		if err := ValidateRoomKey(key); err != nil {
			t.Errorf("%q: err = %v, want nil", key, err)
		}
	}
}

func TestPushRateLimitIsSentinel(t *testing.T) {
	err := error(&PushRateLimit{RetryAfter: 30 * time.Second})
	if !errors.Is(err, ErrPushRateLimited) {
		t.Fatal("PushRateLimit does not match ErrPushRateLimited")
	}
	if rl, ok := errors.AsType[*PushRateLimit](err); !ok || rl.RetryAfter != 30*time.Second {
		t.Fatalf("AsType = %v, %v", rl, ok)
	}
}
