package ssh

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	gossh "github.com/charmbracelet/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// OwnerRooms is what the `rooms` verb needs: an app's rooms for its owner.
type OwnerRooms interface {
	List(slug domain.Slug, owner domain.Owner) ([]domain.AppRoom, error)
	Remove(slug domain.Slug, owner domain.Owner, ids []domain.RoomID) (service.RoomRemoval, error)
	RemoveAll(slug domain.Slug, owner domain.Owner) (service.RoomRemoval, error)
}

// roomView is one row of `rooms <slug> -o json`.
type roomView struct {
	Room      string `json:"room"`
	CreatedAt string `json:"created_at"`
	ChangedAt string `json:"changed_at"`
}

// roomsRemovedView is the json answer of `rooms <slug> rm`.
type roomsRemovedView struct {
	Removed []string `json:"removed"`
	Absent  []string `json:"absent"`
}

const roomsUsage = "usage: rooms <slug> [-o json] | rooms <slug> rm <room-id>... | rooms <slug> rm --all"

// verbRooms dispatches the owner's room commands:
//
//	rooms <slug>                   list the app's rooms
//	rooms <slug> rm <room-id>...   delete those rooms
//	rooms <slug> rm --all          delete every room of the app
func (s *Server) verbRooms(sess gossh.Session, owner domain.Owner, argv []string) {
	format, rest, err := parseOutputFormat(argv)
	if err != nil {
		emitUsageErr(sess, err)
		return
	}
	slug, err := requireSlug(rest)
	if err != nil {
		emitUsageErr(sess, errors.New(roomsUsage))
		return
	}
	if s.AppRooms == nil {
		emitServiceErr(sess, service.ErrNotFound)
		return
	}
	switch {
	case len(rest) == 1:
		rooms, err := s.AppRooms.List(slug, owner)
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
		if err := writeRooms(sess, sess.Stderr(), format, rooms); err != nil {
			emitServiceErr(sess, err)
			return
		}
	case rest[1] == "rm" && len(rest) > 2:
		removal, err := s.removeRooms(slug, owner, rest[2:])
		if usage, ok := errors.AsType[*roomsUsageError](err); ok {
			emitUsageErr(sess, usage)
			return
		}
		// Rooms removed before a failure stay removed, so they are reported
		// before the error.
		if werr := writeRoomsRemoved(sess, sess.Stderr(), format, removal); werr != nil && err == nil {
			err = werr
		}
		if err != nil {
			emitServiceErr(sess, err)
			return
		}
	default:
		emitUsageErr(sess, errors.New(roomsUsage))
		return
	}
	_ = sess.Exit(ExitOK)
}

// roomsUsageError is a malformed `rooms ... rm` argument list.
type roomsUsageError struct{ msg string }

func (e *roomsUsageError) Error() string { return e.msg }

func (s *Server) removeRooms(slug domain.Slug, owner domain.Owner, args []string) (service.RoomRemoval, error) {
	if len(args) == 1 && args[0] == "--all" {
		return s.AppRooms.RemoveAll(slug, owner)
	}
	ids := make([]domain.RoomID, 0, len(args))
	for _, arg := range args {
		id, err := domain.ParseRoomID(arg)
		if err != nil {
			return service.RoomRemoval{}, &roomsUsageError{fmt.Sprintf("%q is not a room id; `rooms %s` lists them", arg, slug)}
		}
		ids = append(ids, id)
	}
	return s.AppRooms.Remove(slug, owner, ids)
}

func writeRooms(stdout, stderr io.Writer, format outputFormat, rooms []domain.AppRoom) error {
	if format == formatJSON {
		views := make([]roomView, len(rooms))
		for i, room := range rooms {
			views[i] = roomView{
				Room:      room.ID.String(),
				CreatedAt: room.CreatedAt.Format(time.RFC3339),
				ChangedAt: room.TouchedAt.Format(time.RFC3339),
			}
		}
		return writeJSON(stdout, views)
	}
	if len(rooms) == 0 {
		_, _ = fmt.Fprintln(stderr, "no rooms")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ROOM\tCREATED\tCHANGED")
	for _, room := range rooms {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", room.ID,
			room.CreatedAt.Format("2006-01-02 15:04"), room.TouchedAt.Format("2006-01-02 15:04"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "%d of %d rooms (times in UTC).\n", len(rooms), domain.MaxAppRooms)
	return nil
}

func writeRoomsRemoved(stdout, stderr io.Writer, format outputFormat, removal service.RoomRemoval) error {
	if format == formatJSON {
		view := roomsRemovedView{Removed: []string{}, Absent: []string{}}
		for _, id := range removal.Removed {
			view.Removed = append(view.Removed, id.String())
		}
		for _, id := range removal.Absent {
			view.Absent = append(view.Absent, id.String())
		}
		return writeJSON(stdout, view)
	}
	for _, id := range removal.Absent {
		_, _ = fmt.Fprintf(stderr, "%s: no such room\n", id)
	}
	_, _ = fmt.Fprintf(stderr, "removed %d room(s).\n", len(removal.Removed))
	return nil
}
