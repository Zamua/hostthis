package service

import (
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// AppRoomStore lists and removes an app's rooms.
type AppRoomStore interface {
	List(app domain.Slug) ([]domain.AppRoom, error)
	Remove(app domain.Slug, id domain.RoomID, now time.Time) (removed bool, err error)
}

// SlugReader reads the row of a slug, whatever kind of content it holds.
type SlugReader interface {
	Get(slug domain.Slug) (domain.Paste, error)
}

// OwnerRooms lets the owner of an app see and delete its rooms (docs/SPEC.md
// "Owner room deletion"). Anyone can create rooms in an app, so the owner is
// the one who can clear them.
type OwnerRooms struct {
	Slugs SlugReader
	Rooms AppRoomStore
	Now   func() time.Time
}

// RoomRemoval is the outcome of removing rooms from one app.
type RoomRemoval struct {
	Removed []domain.RoomID
	// Absent are ids the app had no room for.
	Absent []domain.RoomID
}

// requireOwner reports ErrNotFound for a slug that does not exist and for one
// that belongs to someone else, so existence never leaks.
func (o *OwnerRooms) requireOwner(slug domain.Slug, owner domain.Owner) error {
	row, err := o.Slugs.Get(slug)
	identity := row.Identity
	// A quarantined app still belongs to its owner, who may clear its rooms.
	if q, ok := errors.AsType[*domain.QuarantinedError](err); ok {
		identity, err = q.Identity, nil
	}
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("owner of %s: %w", slug, err)
	case identity != owner.Identity():
		return ErrNotFound
	}
	return nil
}

// List returns the rooms of an app its owner asks about.
func (o *OwnerRooms) List(slug domain.Slug, owner domain.Owner) ([]domain.AppRoom, error) {
	if err := o.requireOwner(slug, owner); err != nil {
		return nil, err
	}
	return o.Rooms.List(slug)
}

// Remove deletes the named rooms of an app for its owner. It stops at the
// first room that cannot be removed and reports what was removed before it.
func (o *OwnerRooms) Remove(slug domain.Slug, owner domain.Owner, ids []domain.RoomID) (RoomRemoval, error) {
	if err := o.requireOwner(slug, owner); err != nil {
		return RoomRemoval{}, err
	}
	return o.remove(slug, ids)
}

// RemoveAll deletes every room the app's index lists.
func (o *OwnerRooms) RemoveAll(slug domain.Slug, owner domain.Owner) (RoomRemoval, error) {
	rooms, err := o.List(slug, owner)
	if err != nil {
		return RoomRemoval{}, err
	}
	ids := make([]domain.RoomID, len(rooms))
	for i, room := range rooms {
		ids[i] = room.ID
	}
	return o.remove(slug, ids)
}

func (o *OwnerRooms) remove(slug domain.Slug, ids []domain.RoomID) (RoomRemoval, error) {
	var out RoomRemoval
	now := o.Now().UTC()
	for _, id := range ids {
		removed, err := o.Rooms.Remove(slug, id, now)
		if err != nil {
			return out, err
		}
		if removed {
			out.Removed = append(out.Removed, id)
		} else {
			out.Absent = append(out.Absent, id)
		}
	}
	return out, nil
}
