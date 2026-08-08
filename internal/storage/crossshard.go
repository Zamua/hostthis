// Translating the sharded backend's cross-shard refusal into the domain.

package storage

import (
	"errors"
	"fmt"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// translateCrossShard maps shale's cross-shard guard sentinel
// (backend.ErrCrossShard) into the domain vocabulary the deploy service checks
// (domain.ErrCrossShardDeploy), so the service layer never imports a shale
// package. Both sentinels stay in the wrap chain (%w twice) so errors.Is holds
// for either and the log keeps the original text. Any other error passes
// through untouched.
//
// Applied at runAuthoritative, the single point every authoritative write goes
// through, rather than at each caller: a per-caller wrap is one a new write path
// silently omits, and the omission has no symptom until a deploy actually
// straddles shards.
func translateCrossShard(err error) error {
	if err != nil && errors.Is(err, backend.ErrCrossShard) {
		return fmt.Errorf("%w: %w", domain.ErrCrossShardDeploy, err)
	}
	return err
}
