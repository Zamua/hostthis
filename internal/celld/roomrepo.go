package celld

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// RoomRepo is the celld implementation of app room storage.
//
// The room IS the cell, which is what makes the dense per-room sequence free:
// "+1 per committed mutation" needs the mutations serialized, and a cell handles
// one event at a time. On a sharded store the same guarantee costs a CAS per
// write.
//
// The per-APP budget cannot live in a room, because the app spans rooms. It
// lives in the app's paste cell - the app slug already addresses one - alongside
// the creation ledger. So a room write is two cells, and the order is the one
// used everywhere else here: the authoritative state first, the derived total
// after, so a crash between them OVER-charges rather than under-charging.
type RoomRepo struct {
	base   string
	client *http.Client
}

func NewRoomRepo(base string, c *http.Client) *RoomRepo {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &RoomRepo{base: base, client: c}
}

func (r *RoomRepo) paste() *PasteRepo { return &PasteRepo{base: r.base, client: r.client} }

// roomKey addresses the cell. The app slug leads so a room is unreachable
// without knowing which app it belongs to, which is what makes the cross-app
// isolation a property of addressing rather than of a check someone must
// remember to write.
func roomKey(app domain.Slug, id domain.RoomID) string {
	return app.String() + "|" + id.String()
}

func (r *RoomRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	ctx := context.Background()
	if _, err := r.paste().call(ctx, http.MethodPost, "/room/create", "room",
		roomKey(room.AppSlug, room.ID), map[string]any{
			"appSlug": room.AppSlug.String(), "id": room.ID.String(),
			"createdAt": room.CreatedAt.UTC().UnixMilli(),
			"updatedAt": room.UpdatedAt.UTC().UnixMilli(),
		}, nil); err != nil {
		return err
	}
	// The ledger entry follows the room, so a crash between them under-counts
	// the rate limit for one creation rather than recording a room that does
	// not exist.
	_, err := r.paste().call(ctx, http.MethodPost, "/paste/roomcreated", "slug",
		room.AppSlug.String(), map[string]any{
			"id": room.ID.String(), "subnet": subnet, "at": now.UTC().UnixMilli(),
		}, nil)
	return err
}

func (r *RoomRepo) GetRoom(app domain.Slug, id domain.RoomID) (domain.Room, error) {
	var wire struct {
		AppSlug   string `json:"appSlug"`
		ID        string `json:"id"`
		CreatedAt int64  `json:"createdAt"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	status, err := r.paste().call(context.Background(), http.MethodGet, "/room/meta", "room",
		roomKey(app, id), nil, &wire)
	if err != nil {
		return domain.Room{}, err
	}
	if status == http.StatusNotFound {
		return domain.Room{}, domain.ErrNotFound
	}
	return domain.Room{
		AppSlug: app, ID: id,
		CreatedAt: time.UnixMilli(wire.CreatedAt).UTC(),
		UpdatedAt: time.UnixMilli(wire.UpdatedAt).UTC(),
	}, nil
}

func (r *RoomRepo) GetValue(app domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	var wire struct {
		Value []byte `json:"value"`
	}
	status, err := r.paste().call(context.Background(), http.MethodGet,
		"/room/get?key="+urlQuery(key), "room", roomKey(app, id), nil, &wire)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, domain.ErrNotFound
	}
	// A stored empty value is a real value, and []byte(nil) and []byte{} are
	// indistinguishable to a caller comparing bytes - but not to one comparing
	// against nil, so the non-nil empty slice is returned deliberately.
	if wire.Value == nil {
		return []byte{}, nil
	}
	return wire.Value, nil
}

func (r *RoomRepo) ScanRoom(app domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	var wire struct {
		Values map[string][]byte `json:"values"`
		Seq    uint64            `json:"seq"`
	}
	status, err := r.paste().call(context.Background(), http.MethodGet, "/room/scan", "room",
		roomKey(app, id), nil, &wire)
	if err != nil {
		return domain.RoomKV{}, err
	}
	if status == http.StatusNotFound {
		return domain.RoomKV{}, domain.ErrNotFound
	}
	out := domain.RoomKV{Values: make(map[string][]byte, len(wire.Values)), Seq: wire.Seq}
	for k, v := range wire.Values {
		if v == nil {
			v = []byte{}
		}
		out.Values[k] = v
	}
	return out, nil
}

// PutValue writes one key, enforcing the per-room caps and the app's budget.
//
// Both caps are decided inside the room cell's single event, so a rejected
// write leaves the prior state untouched. The app's other-room bytes are read
// first and handed in: that figure can be stale if a sibling room is written
// concurrently, a bounded overshoot on the APP cap only. The per-room cap is
// exact because the room's own bytes are read in the same event that writes.
func (r *RoomRepo) PutValue(app domain.Slug, id domain.RoomID, key string, val []byte,
	appCap int64, now time.Time,
) (uint64, error) {
	ctx := context.Background()
	other, err := r.otherRoomBytes(ctx, app, id)
	if err != nil {
		return 0, err
	}
	var res struct {
		Seq   uint64 `json:"seq"`
		Bytes int    `json:"bytes"`
	}
	status, err := r.paste().call(ctx, http.MethodPost, "/room/put", "room", roomKey(app, id),
		map[string]any{
			"key": key, "value": base64.StdEncoding.EncodeToString(val),
			"roomCap": domain.MaxRoomBytes, "keyCap": domain.MaxRoomKeys,
			"appCap": appCap, "otherBytes": other, "now": now.UTC().UnixMilli(),
		}, &res)
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusNotFound:
		return 0, domain.ErrNotFound
	case http.StatusRequestEntityTooLarge:
		return 0, domain.ErrRoomDataFull
	case http.StatusInsufficientStorage:
		return 0, domain.ErrAppRoomsFull
	}
	if status >= 300 {
		return 0, fmt.Errorf("celld: room put: unexpected status %d", status)
	}
	return res.Seq, r.settle(ctx, app, id, res.Bytes)
}

func (r *RoomRepo) DeleteValue(app domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	ctx := context.Background()
	var res struct {
		Seq   uint64 `json:"seq"`
		Bytes int    `json:"bytes"`
	}
	status, err := r.paste().call(ctx, http.MethodPost, "/room/del", "room", roomKey(app, id),
		map[string]any{"key": key, "now": now.UTC().UnixMilli()}, &res)
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound {
		return 0, domain.ErrNotFound
	}
	if status >= 300 {
		return 0, fmt.Errorf("celld: room delete: unexpected status %d", status)
	}
	return res.Seq, r.settle(ctx, app, id, res.Bytes)
}

// CountRoomCreates reads the app's creation ledger, which also prunes it.
func (r *RoomRepo) CountRoomCreates(app domain.Slug, subnet string, now time.Time,
	window time.Duration,
) (int, int, error) {
	var res struct {
		PerSubnet int `json:"perSubnet"`
		PerApp    int `json:"perApp"`
	}
	u := fmt.Sprintf("/paste/roomcounts?subnet=%s&now=%d&window=%d",
		urlQuery(subnet), now.UTC().UnixMilli(), window.Milliseconds())
	if _, err := r.paste().call(context.Background(), http.MethodGet, u, "slug",
		app.String(), nil, &res); err != nil {
		return 0, 0, err
	}
	return res.PerSubnet, res.PerApp, nil
}

func (r *RoomRepo) otherRoomBytes(ctx context.Context, app domain.Slug, id domain.RoomID) (int, error) {
	var res struct {
		OtherBytes int `json:"otherBytes"`
	}
	u := "/paste/roomothers?room=" + urlQuery(id.String())
	if _, err := r.paste().call(ctx, http.MethodGet, u, "slug", app.String(), nil, &res); err != nil {
		return 0, err
	}
	return res.OtherBytes, nil
}

// settle records the room's ABSOLUTE total against the app. Absolute, not a
// delta, so a retry is a no-op where "add n" would double-charge.
func (r *RoomRepo) settle(ctx context.Context, app domain.Slug, id domain.RoomID, bytes int) error {
	_, err := r.paste().call(ctx, http.MethodPost, "/paste/roomsettle", "slug", app.String(),
		map[string]any{"room": id.String(), "bytes": bytes}, nil)
	return err
}
