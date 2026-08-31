// The package's shared persistence vocabulary.
package storage

import "github.com/Zamua/hostthis/internal/domain"

var (
	ErrNotFound       = domain.ErrNotFound
	ErrServiceFull    = domain.ErrServiceFull
	ErrTooManyNewKeys = domain.ErrTooManyNewKeys
	ErrSlugTaken      = domain.ErrSlugTaken
	ErrAppRoomsFull   = domain.ErrAppRoomsFull
	ErrRoomDataFull   = domain.ErrRoomDataFull
	ErrOverUserQuota  = domain.ErrOverUserQuota
)

type AppendResult = domain.AppendResult
