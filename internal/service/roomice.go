package service

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// RoomICERepo is the persistence contract for a room's WebRTC ICE servers
// (SPEC.md "TURN relay credentials").
type RoomICERepo interface {
	// GetRoom returns domain.ErrNotFound for a missing room.
	GetRoom(app domain.Slug, id domain.RoomID) (domain.Room, error)
	// IssueICE returns the TURN cell's answer, the JSON document the route
	// serves: STUN always, and a TURN credential when one is issued.
	IssueICE(app domain.Slug, id domain.RoomID, now time.Time) (json.RawMessage, error)
}

// RoomICE hands a room participant its ICE servers. The TURN cell decides
// everything about TURN; this service only checks the room exists first.
type RoomICE struct {
	Repo RoomICERepo
	Now  func() time.Time
}

func NewRoomICE(repo RoomICERepo) *RoomICE {
	return &RoomICE{Repo: repo, Now: time.Now}
}

// Servers returns the room's ICE servers document. ErrRoomNotFound for a
// missing room.
func (s *RoomICE) Servers(app domain.Slug, id domain.RoomID) (json.RawMessage, error) {
	if _, err := s.Repo.GetRoom(app, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, ErrRoomNotFound
		}
		return nil, err
	}
	return s.Repo.IssueICE(app, id, s.Now())
}
