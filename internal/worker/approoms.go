package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// AppRoomsRepo lists and removes the rooms of one app for its owner. The app's
// Paste cell holds the room index; each Room cell removes itself.
type AppRoomsRepo struct {
	*cell
}

func NewAppRoomsRepo(base string, c *http.Client) *AppRoomsRepo {
	return &AppRoomsRepo{cell: newCell(base, c)}
}

// List returns every room the app's index holds, in id order.
func (r *AppRoomsRepo) List(app domain.Slug) ([]domain.AppRoom, error) {
	var rooms []domain.AppRoom
	start := ""
	for {
		var page struct {
			Rooms []struct {
				Room      string `json:"room"`
				CreatedAt int64  `json:"createdAt"`
				TouchedAt int64  `json:"touchedAt"`
			} `json:"rooms"`
			Next *string `json:"next"`
		}
		rt := opPasteRoomIndex
		if start != "" {
			rt = rt.withQuery("start=" + url.QueryEscape(start))
		}
		status, err := r.call(context.Background(), rt, app.String(), nil, &page)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("worker: room index: status %d", status)
		}
		for _, e := range page.Rooms {
			id, err := domain.ParseRoomID(e.Room)
			if err != nil {
				continue // an index entry that names no room cannot be listed or removed
			}
			rooms = append(rooms, domain.AppRoom{
				ID: id, CreatedAt: time.UnixMilli(e.CreatedAt).UTC(), TouchedAt: time.UnixMilli(e.TouchedAt).UTC(),
			})
		}
		if page.Next == nil || *page.Next == "" {
			return rooms, nil
		}
		start = *page.Next
	}
}

// Remove deletes one room: its bytes are released, it stops counting toward
// the app's room limit, its sockets are closed and its scheduled pushes go
// with it. removed is false when the app has no such room. A removal the
// cells could not finish fails with domain.ErrRoomRemovalIncomplete and
// completes when retried.
func (r *AppRoomsRepo) Remove(app domain.Slug, id domain.RoomID, now time.Time) (removed bool, err error) {
	var res struct {
		Removed bool   `json:"removed"`
		Error   string `json:"error"`
	}
	status, err := r.call(context.Background(), opRoomRemove, roomKey(app, id),
		struct {
			Now int64 `json:"now"`
		}{now.UTC().UnixMilli()}, &res)
	if status == http.StatusBadGateway {
		return false, domain.ErrRoomRemovalIncomplete
	}
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("worker: room remove: status %d %s", status, res.Error)
	}
	return res.Removed, nil
}
