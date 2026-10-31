package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// RoomRepo stores each room in one Worker cell. The app's Paste cell coordinates
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
// without its app: cross-app isolation is a property of addressing, not of a
// check someone must remember to write.
func roomKey(app domain.Slug, id domain.RoomID) string {
	return app.String() + "|" + id.String()
}

// CreateRoom records the room and its app-scoped creation ledger through one
// Room-cell request. A cell that already holds the room answers
// {created:false}: domain.ErrSlugTaken, so the caller retries a fresh id.
func (r *RoomRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	var res struct {
		Created *bool `json:"created"`
	}
	if err := r.ask(context.Background(), "room create", opRoomCreate,
		roomKey(room.AppSlug, room.ID), roomCreateRequest{
			AppSlug: room.AppSlug.String(), ID: room.ID.String(),
			CreatedAt: room.CreatedAt.UTC().UnixMilli(),
			UpdatedAt: room.UpdatedAt.UTC().UnixMilli(),
			Subnet:    subnet, At: now.UTC().UnixMilli(),
			AppCap: appCap,
		}, &res, map[int]error{http.StatusInsufficientStorage: domain.ErrAppRoomsFull}); err != nil {
		return err
	}
	if res.Created != nil && !*res.Created {
		return domain.ErrSlugTaken
	}
	return nil
}

func (r *RoomRepo) GetRoom(app domain.Slug, id domain.RoomID) (domain.Room, error) {
	var wire struct {
		AppSlug   string `json:"appSlug"`
		ID        string `json:"id"`
		CreatedAt int64  `json:"createdAt"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	status, err := r.call(context.Background(), opRoomMeta, roomKey(app, id), nil, &wire)
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
	status, err := r.call(context.Background(), opRoomGet.withQuery("key="+urlQuery(key)),
		roomKey(app, id), nil, &wire)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, domain.ErrNotFound
	}
	// A stored empty value is a real value; the non-nil empty slice keeps it
	// distinguishable from absence for callers comparing against nil.
	if wire.Value == nil {
		return []byte{}, nil
	}
	return wire.Value, nil
}

func (r *RoomRepo) ScanRoom(app domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	var wire struct {
		Values map[string][]byte `json:"values"`
		Seq    uint64            `json:"seq"`
		// The text the cell holds per value, which its frames carry.
		Wire map[string]string `json:"wire"`
	}
	status, err := r.call(context.Background(), opRoomScan, roomKey(app, id), nil, &wire)
	if err != nil {
		return domain.RoomKV{}, err
	}
	if status == http.StatusNotFound {
		return domain.RoomKV{}, domain.ErrNotFound
	}
	out := domain.RoomKV{
		Values: make(map[string][]byte, len(wire.Values)), Seq: wire.Seq,
		Wire: make(map[string]json.RawMessage, len(wire.Wire)),
	}
	for k, v := range wire.Values {
		if v == nil {
			v = []byte{}
		}
		out.Values[k] = v
		// Text that is not JSON is left out, to be encoded from the bytes,
		// rather than spliced into every reader's document.
		if text, ok := wire.Wire[k]; ok && json.Valid([]byte(text)) {
			out.Wire[k] = json.RawMessage(text)
		}
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
	if err := r.ask(context.Background(), "room put", opRoomPut, roomKey(app, id),
		roomPutRequest{
			Key: key, Value: base64.StdEncoding.EncodeToString(val),
			// The frame encoding computed HERE, with the same encoder the HTTP
			// handlers use: the cell echoes it into snapshots and mirror frames
			// without interpreting the bytes, so the subtle raw-JSON-or-string
			// rule exists in exactly one place.
			Wire:    string(domain.RoomWireValue(val)),
			RoomCap: domain.MaxRoomBytes, KeyCap: domain.MaxRoomKeys,
			AppCap: appCap, Now: now.UTC().UnixMilli(),
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
	if err := r.ask(context.Background(), "room delete", opRoomDel,
		roomKey(app, id), roomDelRequest{Key: key, Now: now.UTC().UnixMilli()}, &res, notFound); err != nil {
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
	q := fmt.Sprintf("subnet=%s&now=%d&window=%d",
		urlQuery(subnet), now.UTC().UnixMilli(), window.Milliseconds())
	if _, err := r.call(context.Background(), opPasteRoomCounts.withQuery(q), app.String(), nil, &res); err != nil {
		return 0, 0, err
	}
	return res.PerSubnet, res.PerApp, nil
}
