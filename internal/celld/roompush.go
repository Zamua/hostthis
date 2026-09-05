package celld

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// The push surface of RoomRepo. Every room-scoped op is one POST to the Room
// cell addressed like the KV ops; the VAPID key lives with the app's Paste
// cell. The Go domain validates before any call, so a cell 400, or a 413
// whose error code is not the cap the caller can act on, is a contract
// disagreement and surfaces as a plain error.

// PushKey asks the app cell for its VAPID public key, generated on first use.
func (r *RoomRepo) PushKey(app domain.Slug) (string, error) {
	var res struct {
		Key string `json:"key"`
	}
	if err := r.ask(context.Background(), "push key", http.MethodPost, "/paste/pushkey", "slug",
		app.String(), nil, &res, notFound); err != nil {
		return "", err
	}
	return res.Key, nil
}

// PutPushSubscription sends the browser's subscription fields plus the VAPID
// subject the cell signs tokens with.
func (r *RoomRepo) PutPushSubscription(app domain.Slug, id domain.RoomID, sub domain.PushSubscription,
	now time.Time,
) error {
	status, code, err := r.pushPut("/room/pushsubput", app, id, map[string]any{
		"endpoint": sub.Endpoint,
		"keys":     map[string]string{"p256dh": sub.Keys.P256DH, "auth": sub.Keys.Auth},
		"now":      now.UTC().UnixMilli(), "subject": r.PushSubject,
	})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNotFound:
		return domain.ErrNotFound
	case status == http.StatusRequestEntityTooLarge && code == "too-many-subscriptions":
		return domain.ErrPushFull
	case status >= 300:
		return fmt.Errorf("celld: push subscription put: unexpected status %d (%s)", status, code)
	}
	return nil
}

// pushPut posts one body to a Room cell push op and returns the status with
// the cell's error code, the `error` field a refusal carries. Only the code
// tells a cap the caller can act on from a contract fault at the same status.
func (r *RoomRepo) pushPut(path string, app domain.Slug, id domain.RoomID, body any) (int, string, error) {
	resp, err := r.do(context.Background(), http.MethodPost, path, "room", roomKey(app, id), body)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var refusal struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &refusal)
	return resp.StatusCode, refusal.Error, nil
}

func (r *RoomRepo) DeletePushSubscription(app domain.Slug, id domain.RoomID, endpoint string) error {
	return r.ask(context.Background(), "push subscription delete", http.MethodPost, "/room/pushsubdel",
		"room", roomKey(app, id), map[string]string{"endpoint": endpoint}, nil, notFound)
}

func (r *RoomRepo) ListPushSubscriptions(app domain.Slug, id domain.RoomID) ([]domain.PushSubscriptionSummary, error) {
	var wire struct {
		Subscriptions []struct {
			Endpoint string   `json:"endpoint"`
			Added    wireTime `json:"added"`
		} `json:"subscriptions"`
	}
	if err := r.ask(context.Background(), "push subscription list", http.MethodPost, "/room/pushsublist",
		"room", roomKey(app, id), nil, &wire, notFound); err != nil {
		return nil, err
	}
	out := make([]domain.PushSubscriptionSummary, 0, len(wire.Subscriptions))
	for _, w := range wire.Subscriptions {
		out = append(out, domain.PushSubscriptionSummary{Endpoint: w.Endpoint, Added: time.Time(w.Added)})
	}
	return out, nil
}

func (r *RoomRepo) PutPushSchedule(app domain.Slug, id domain.RoomID, sched domain.PushSchedule,
	now time.Time,
) error {
	items := sched.Items
	if items == nil {
		items = []domain.PushItem{}
	}
	status, code, err := r.pushPut("/room/pushscheduleput", app, id, map[string]any{
		"tz": sched.TZ, "items": items, "now": now.UTC().UnixMilli(), "subject": r.PushSubject,
	})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNotFound:
		return domain.ErrNotFound
	case status == http.StatusRequestEntityTooLarge && code == "too-many-items":
		return domain.ErrPushFull
	case status >= 300:
		return fmt.Errorf("celld: push schedule put: unexpected status %d (%s)", status, code)
	}
	return nil
}

func (r *RoomRepo) GetPushSchedule(app domain.Slug, id domain.RoomID) (domain.PushSchedule, error) {
	var sched domain.PushSchedule
	if err := r.ask(context.Background(), "push schedule get", http.MethodPost, "/room/pushscheduleget",
		"room", roomKey(app, id), nil, &sched, notFound); err != nil {
		return domain.PushSchedule{}, err
	}
	if sched.Items == nil {
		sched.Items = []domain.PushItem{}
	}
	return sched, nil
}

// TestPush runs the cell's inline test send. A 429 carries the wait in its
// body, which is why this reads the response itself rather than through call.
func (r *RoomRepo) TestPush(app domain.Slug, id domain.RoomID, now time.Time) (domain.PushTestResult, error) {
	resp, err := r.do(context.Background(), http.MethodPost, "/room/pushtest", "room",
		roomKey(app, id), map[string]any{"now": now.UTC().UnixMilli()})
	if err != nil {
		return domain.PushTestResult{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusNotFound:
		return domain.PushTestResult{}, domain.ErrNotFound
	case http.StatusTooManyRequests:
		rl := &domain.PushRateLimit{RetryAfter: domain.PushTestInterval}
		var wire struct {
			RetryAfter float64 `json:"retryAfter"`
		}
		if json.Unmarshal(body, &wire) == nil && wire.RetryAfter > 0 {
			rl.RetryAfter = time.Duration(wire.RetryAfter * float64(time.Second))
		}
		return domain.PushTestResult{}, rl
	}
	if resp.StatusCode >= 300 {
		return domain.PushTestResult{}, fmt.Errorf("celld: push test: status %d: %s", resp.StatusCode, body)
	}
	var res domain.PushTestResult
	if len(body) > 0 {
		if err := json.Unmarshal(body, &res); err != nil {
			return domain.PushTestResult{}, fmt.Errorf("celld: decode push test: %w", err)
		}
	}
	return res, nil
}

// wireTime decodes a cell timestamp given either as RFC 3339 or as epoch
// milliseconds, the two forms cell ops emit.
type wireTime time.Time

func (t *wireTime) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		parsed, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return err
		}
		*t = wireTime(parsed.UTC())
		return nil
	}
	ms, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return err
	}
	*t = wireTime(time.UnixMilli(ms).UTC())
	return nil
}
