// The package's shared persistence vocabulary.
package storage

import "github.com/Zamua/hostthis/internal/domain"

var (
	ErrNotFound       = domain.ErrNotFound
	ErrTooManyNewKeys = domain.ErrTooManyNewKeys
	ErrSlugTaken      = domain.ErrSlugTaken
	ErrOverUserQuota  = domain.ErrOverUserQuota
)

type AppendResult = domain.AppendResult
