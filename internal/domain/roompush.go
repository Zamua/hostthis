package domain

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Room push (SPEC.md "Room push (scheduled Web Push)"): a room holds a set of
// browser push subscriptions and a schedule of notifications. The domain owns
// the shapes and every validation rule; delivery is the backend's.

// Caps.
const (
	MaxPushSubscriptions = 16
	MaxPushItems         = 16
	MaxPushPayloadBytes  = 2048
	MaxPushSendsPerDay   = 8
	MaxPushEndpointBytes = 1024
	MaxPushIDLen         = 32
	MaxPushTitleBytes    = 64
	MaxPushURLBytes      = 512
	MaxPushTagBytes      = 64
	MaxPushBodyBytes     = 1024
	// PushTestInterval spaces test sends per room.
	PushTestInterval = time.Minute
)

var (
	// ErrPushInvalid wraps every validation failure of a subscription or a
	// schedule. HTTP 400 with the wrapped detail.
	ErrPushInvalid = errors.New("push: invalid")
	// ErrPushFull is returned when a room is at MaxPushSubscriptions and the
	// endpoint is new. HTTP 413.
	ErrPushFull = errors.New("push: room is at its subscription cap")
	// ErrPushRateLimited is the test-send rate limit. *PushRateLimit carries
	// the wait. HTTP 429.
	ErrPushRateLimited = errors.New("push: test rate limit reached")
	// ErrRoomKeyReserved is returned by ValidateRoomKey for keys the room API
	// serves as something other than data.
	ErrRoomKeyReserved = errors.New("room key is reserved")
)

// PushRateLimit enriches ErrPushRateLimited with the wait until the next test
// send is admitted.
type PushRateLimit struct{ RetryAfter time.Duration }

func (e *PushRateLimit) Error() string        { return ErrPushRateLimited.Error() }
func (e *PushRateLimit) Is(target error) bool { return target == ErrPushRateLimited }

// PushSubscription is the browser's PushSubscription JSON, keys kept in their
// base64url wire form so the backend hands them on untouched.
type PushSubscription struct {
	Endpoint string   `json:"endpoint"`
	Keys     PushKeys `json:"keys"`
}

// PushKeys are the subscriber's ECDH point and auth secret, base64url.
type PushKeys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// PushSubscriptionSummary is the listable view: no keys leave the backend.
type PushSubscriptionSummary struct {
	Endpoint string    `json:"endpoint"`
	Added    time.Time `json:"added"`
}

// PushTestResult reports one inline test send.
type PushTestResult struct {
	Sent   int `json:"sent"`
	Pruned int `json:"pruned"`
}

// pushServiceBases is the endpoint allowlist: a host passes when it equals a
// base or ends with "." plus a base. Everything else, including any literal
// IP, is refused so a room can never aim ciphertext and a VAPID token at an
// arbitrary server.
var pushServiceBases = []string{"push.apple.com", "fcm.googleapis.com", "push.services.mozilla.com", "notify.windows.com"}

// pushServiceHost reports whether host (lowercased, no port) belongs to a
// known push service.
func pushServiceHost(host string) bool {
	for _, base := range pushServiceBases {
		if host == base || strings.HasSuffix(host, "."+base) {
			return true
		}
	}
	return false
}

// ValidatePushSubscription applies the wire-shape rules: an https endpoint of
// at most MaxPushEndpointBytes on a known push service, a 65-byte uncompressed
// P-256 point, a 16-byte auth secret.
func ValidatePushSubscription(s PushSubscription) error {
	if s.Endpoint == "" {
		return fmt.Errorf("%w: endpoint is required", ErrPushInvalid)
	}
	if len(s.Endpoint) > MaxPushEndpointBytes {
		return fmt.Errorf("%w: endpoint exceeds %d bytes", ErrPushInvalid, MaxPushEndpointBytes)
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: endpoint must be an https URL", ErrPushInvalid)
	}
	if u.User != nil || !pushServiceHost(strings.ToLower(u.Hostname())) {
		return fmt.Errorf("%w: endpoint host is not a known push service", ErrPushInvalid)
	}
	for _, c := range s.Endpoint {
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("%w: endpoint contains a control character", ErrPushInvalid)
		}
	}
	p, err := decodeBase64URL(s.Keys.P256DH)
	if err != nil || len(p) != 65 || p[0] != 0x04 {
		return fmt.Errorf("%w: keys.p256dh must be a base64url uncompressed P-256 point", ErrPushInvalid)
	}
	a, err := decodeBase64URL(s.Keys.Auth)
	if err != nil || len(a) != 16 {
		return fmt.Errorf("%w: keys.auth must be 16 base64url bytes", ErrPushInvalid)
	}
	return nil
}

// decodeBase64URL accepts the unpadded form browsers emit and the padded one.
func decodeBase64URL(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// PushSchedule is a room's whole schedule; PUT replaces it entirely.
type PushSchedule struct {
	TZ    string     `json:"tz"`
	Items []PushItem `json:"items"`
}

// PushItem is one notification rule: recurring (At + Days, in the schedule's
// TZ) or one-shot (When). Exactly one of Body / BodyKey carries the text.
type PushItem struct {
	ID      string `json:"id"`
	At      string `json:"at,omitempty"`
	Days    []int  `json:"days,omitempty"`
	When    string `json:"when,omitempty"`
	Title   string `json:"title"`
	Body    string `json:"body,omitempty"`
	BodyKey string `json:"bodyKey,omitempty"`
	URL     string `json:"url,omitempty"`
	Tag     string `json:"tag,omitempty"`
}

// Recurring reports whether the item is the At + Days shape.
func (it PushItem) Recurring() bool { return it.When == "" }

// WhenTime parses a one-shot item's instant.
func (it PushItem) WhenTime() (time.Time, error) {
	return time.Parse(time.RFC3339, it.When)
}

// pushDateSample stands in for {date} when a BodyKey is validated as a room
// key; the real date is substituted at send time.
const pushDateSample = "2026-09-06"

// ValidatePushSchedule applies every rule in the spec to the whole document;
// a one-shot item must fall strictly after now. The first violation is
// returned; nothing is applied partially.
func ValidatePushSchedule(s PushSchedule, now time.Time) error {
	if len(s.Items) > MaxPushItems {
		return fmt.Errorf("%w: more than %d items", ErrPushInvalid, MaxPushItems)
	}
	if s.TZ == "" {
		return fmt.Errorf("%w: tz is required", ErrPushInvalid)
	}
	// LoadLocation("Local") resolves to the process zone, which no client
	// means by an IANA name.
	if _, err := time.LoadLocation(s.TZ); err != nil || s.TZ == "Local" {
		return fmt.Errorf("%w: unknown tz %q", ErrPushInvalid, s.TZ)
	}
	seen := make(map[string]struct{}, len(s.Items))
	for i, it := range s.Items {
		if err := validatePushItem(it, now); err != nil {
			return fmt.Errorf("%w (item %d)", err, i)
		}
		if _, dup := seen[it.ID]; dup {
			return fmt.Errorf("%w: duplicate id %q", ErrPushInvalid, it.ID)
		}
		seen[it.ID] = struct{}{}
	}
	return nil
}

func validatePushItem(it PushItem, now time.Time) error {
	if it.ID == "" || len(it.ID) > MaxPushIDLen {
		return fmt.Errorf("%w: id must be 1 to %d characters", ErrPushInvalid, MaxPushIDLen)
	}
	for i := 0; i < len(it.ID); i++ {
		if !isPushIDChar(it.ID[i]) {
			return fmt.Errorf("%w: id %q has a character outside [A-Za-z0-9_-]", ErrPushInvalid, it.ID)
		}
	}
	if it.Title == "" || len(it.Title) > MaxPushTitleBytes {
		return fmt.Errorf("%w: title must be 1 to %d bytes", ErrPushInvalid, MaxPushTitleBytes)
	}
	if len(it.URL) > MaxPushURLBytes {
		return fmt.Errorf("%w: url exceeds %d bytes", ErrPushInvalid, MaxPushURLBytes)
	}
	if len(it.Tag) > MaxPushTagBytes {
		return fmt.Errorf("%w: tag exceeds %d bytes", ErrPushInvalid, MaxPushTagBytes)
	}
	switch {
	case it.Body != "" && it.BodyKey != "", it.Body == "" && it.BodyKey == "":
		return fmt.Errorf("%w: exactly one of body and bodyKey is required", ErrPushInvalid)
	case it.Body != "":
		if len(it.Body) > MaxPushBodyBytes {
			return fmt.Errorf("%w: body exceeds %d bytes", ErrPushInvalid, MaxPushBodyBytes)
		}
	default:
		key := strings.ReplaceAll(it.BodyKey, "{date}", pushDateSample)
		if err := ValidateRoomKey(key); err != nil {
			return fmt.Errorf("%w: bodyKey: %v", ErrPushInvalid, err)
		}
	}
	if it.When != "" {
		if it.At != "" || len(it.Days) > 0 {
			return fmt.Errorf("%w: when excludes at and days", ErrPushInvalid)
		}
		when, err := it.WhenTime()
		if err != nil {
			return fmt.Errorf("%w: when must be RFC 3339 with an offset", ErrPushInvalid)
		}
		if !when.After(now) {
			return fmt.Errorf("%w: when is not in the future", ErrPushInvalid)
		}
		return nil
	}
	if !isPushClock(it.At) {
		return fmt.Errorf("%w: at must be HH:MM", ErrPushInvalid)
	}
	if len(it.Days) == 0 {
		return fmt.Errorf("%w: days must name at least one weekday", ErrPushInvalid)
	}
	var mask uint8
	for _, d := range it.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("%w: day %d is outside 0..6", ErrPushInvalid, d)
		}
		if mask&(1<<d) != 0 {
			return fmt.Errorf("%w: day %d repeats", ErrPushInvalid, d)
		}
		mask |= 1 << d
	}
	return nil
}

func isPushIDChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// isPushClock matches exactly HH:MM, 00:00 through 23:59.
func isPushClock(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	return h < 24 && m < 60
}
