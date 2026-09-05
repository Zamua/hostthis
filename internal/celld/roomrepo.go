package celld

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// RoomRepo stores each room in one celld cell. The app's Paste cell coordinates
// exact byte allocations across sibling rooms.
type RoomRepo struct {
	*cell
	// PushSubject is the VAPID `sub` claim ("https://<apex>") handed to the
	// cell with every push write, so its tokens name this deployment.
	PushSubject string
}

func NewRoomRepo(base string, c *http.Client) *RoomRepo {
	return &RoomRepo{cell: newCell(base, c)}
}

// roomKey addresses the cell. The app slug leads so a room is unreachable
// without knowing which app it belongs to, which is what makes the cross-app
// isolation a property of addressing rather than of a check someone must
// remember to write.
func roomKey(app domain.Slug, id domain.RoomID) string {
	return app.String() + "|" + id.String()
}

// CreateRoom records the room and its app-scoped creation ledger through one
// Room-cell request.
func (r *RoomRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	return r.ask(context.Background(), "room create", http.MethodPost, "/room/create", "room",
		roomKey(room.AppSlug, room.ID), map[string]any{
			"appSlug": room.AppSlug.String(), "id": room.ID.String(),
			"createdAt": room.CreatedAt.UTC().UnixMilli(),
			"updatedAt": room.UpdatedAt.UTC().UnixMilli(),
			"subnet":    subnet, "at": now.UTC().UnixMilli(),
			"appCap": appCap,
		}, nil, map[int]error{http.StatusInsufficientStorage: domain.ErrAppRoomsFull})
}

func (r *RoomRepo) GetRoom(app domain.Slug, id domain.RoomID) (domain.Room, error) {
	var wire struct {
		AppSlug   string `json:"appSlug"`
		ID        string `json:"id"`
		CreatedAt int64  `json:"createdAt"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	status, err := r.call(context.Background(), http.MethodGet, "/room/meta", "room",
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
	status, err := r.call(context.Background(), http.MethodGet,
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
	status, err := r.call(context.Background(), http.MethodGet, "/room/scan", "room",
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

// PutValue writes one key: ONE call. The room cell enforces its own caps in
// its event and asks the app's paste cell for the per-app budget CELL TO CELL,
// so that cap is exact and a rejected write leaves the prior state untouched.
func (r *RoomRepo) PutValue(app domain.Slug, id domain.RoomID, key string, val []byte,
	appCap int64, now time.Time,
) (uint64, error) {
	var res struct {
		Seq   uint64 `json:"seq"`
		Bytes int    `json:"bytes"`
	}
	if err := r.ask(context.Background(), "room put", http.MethodPost, "/room/put", "room", roomKey(app, id),
		map[string]any{
			"key": key, "value": base64.StdEncoding.EncodeToString(val),
			// The frame encoding computed HERE, with the same encoder the HTTP
			// handlers use: the cell echoes it into snapshots and mirror frames
			// without interpreting the bytes, so the subtle raw-JSON-or-string
			// rule exists in exactly one place.
			"wire":    string(domain.RoomWireValue(val)),
			"roomCap": domain.MaxRoomBytes, "keyCap": domain.MaxRoomKeys,
			"appCap": appCap, "now": now.UTC().UnixMilli(),
		}, &res, map[int]error{
			http.StatusNotFound:              domain.ErrNotFound,
			http.StatusRequestEntityTooLarge: domain.ErrRoomDataFull,
			http.StatusInsufficientStorage:   domain.ErrAppRoomsFull,
		}); err != nil {
		return 0, err
	}
	return res.Seq, nil
}

func (r *RoomRepo) DeleteValue(app domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	var res struct {
		Seq   uint64 `json:"seq"`
		Bytes int    `json:"bytes"`
	}
	if err := r.ask(context.Background(), "room delete", http.MethodPost, "/room/del", "room",
		roomKey(app, id), map[string]any{"key": key, "now": now.UTC().UnixMilli()}, &res, notFound); err != nil {
		return 0, err
	}
	return res.Seq, nil
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
	if _, err := r.call(context.Background(), http.MethodGet, u, "slug",
		app.String(), nil, &res); err != nil {
		return 0, 0, err
	}
	return res.PerSubnet, res.PerApp, nil
}
