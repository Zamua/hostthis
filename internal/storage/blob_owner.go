// The fence that stops a writer recovery took over from binding bytes recovery
// is about to delete.

package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// ErrFenced reports that the caller's blob-ownership epoch is stale: recovery
// took this upload over and its bytes are gone or going. The write must abort.
//
// It is not a transient - retrying with the same epoch can only fail again, and
// retrying with a fresh one would bind bytes that no longer exist.
var ErrFenced = errors.New("storage: upload fenced; recovery took it over")

// blobOwnerEpochKey is the unexported context key the claimed epoch rides
// under, mirroring how staged refs reach the binding transaction: the value is
// request state, and threading it through every signature between the upload
// and the commit would put it in the vocabulary of code that has no use for it.
type blobOwnerEpochKey struct{}

// WithBlobOwnerEpoch returns a child context carrying the epoch this upload
// claimed, for the authoritative write to check as it binds.
func WithBlobOwnerEpoch(ctx context.Context, epoch int64) context.Context {
	if epoch == 0 {
		return ctx
	}
	return context.WithValue(ctx, blobOwnerEpochKey{}, epoch)
}

// blobOwnerEpochFromContext returns the claimed epoch, or 0 when the caller
// staged no blobs and so claimed nothing.
func blobOwnerEpochFromContext(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	epoch, _ := ctx.Value(blobOwnerEpochKey{}).(int64)
	return epoch
}

// shaleKeyBlobOwner builds blobowner/<slug>, which shards on the SLUG so it
// co-shards with the bref it guards: the check runs inside the bind's
// transaction, and that transaction is pinned on the slug.
func shaleKeyBlobOwner(slug domain.Slug) []byte {
	return []byte("blobowner/" + slug.String())
}

// ClaimBlobOwnership takes ownership of a slug's staged bytes and returns the
// epoch the caller must present at bind time.
//
// Called BEFORE the first byte is staged. A fresh slug starts at epoch 1; a slug
// whose previous attempt was taken over returns whatever recovery left, so the
// new attempt is fenced against the old one rather than inheriting its identity.
func (r *ShaleRepo) ClaimBlobOwnership(_ context.Context, slug domain.Slug) (int64, error) {
	key := shaleKeyBlobOwner(slug)
	var epoch int64
	err := r.cluster.Transact(key, func(tx backend.Transaction) error {
		cur, gerr := txReadEpoch(tx, key)
		if gerr != nil {
			return gerr
		}
		if cur == 0 {
			cur = 1
			if perr := tx.Put(key, []byte(strconv.FormatInt(cur, 10))); perr != nil {
				return perr
			}
		}
		epoch = cur
		return nil
	})
	return epoch, err
}

// FenceBlobOwnership bumps the epoch so the writer that claimed it can no longer
// bind, and returns the new value.
//
// Recovery calls this BEFORE unstaging anything. The order is the whole
// mechanism: bump-then-delete makes a resumed writer's bind abort with
// certainty, where delete-then-bump leaves a window in which it binds bytes that
// are already gone.
func (r *ShaleRepo) FenceBlobOwnership(_ context.Context, slug domain.Slug) (int64, error) {
	key := shaleKeyBlobOwner(slug)
	var next int64
	err := r.cluster.Transact(key, func(tx backend.Transaction) error {
		cur, gerr := txReadEpoch(tx, key)
		if gerr != nil {
			return gerr
		}
		next = cur + 1
		return tx.Put(key, []byte(strconv.FormatInt(next, 10)))
	})
	return next, err
}

// checkBlobOwnership verifies inside a caller's transaction that epoch is still
// the live one, returning ErrFenced when it is not.
//
// Takes the transaction rather than opening its own: the check is only worth
// anything if it commits atomically with the bind it guards. A separate read
// would re-open the window it exists to close.
//
// epoch 0 means the caller never claimed ownership - the paths that do not stage
// blobs at all - and skips the check rather than failing it.
func checkBlobOwnership(tx shaleKVTx, slug domain.Slug, epoch int64) error {
	if epoch == 0 {
		return nil
	}
	key := shaleKeyBlobOwner(slug)
	cur, err := txReadEpoch(tx, key)
	if err != nil {
		return err
	}
	if cur != epoch {
		return fmt.Errorf("%w: slug %s epoch %d, live epoch %d", ErrFenced, slug, epoch, cur)
	}
	return nil
}

// txReadEpoch reads the epoch inside a transaction, reporting 0 for absent.
//
// The read JOINS the transaction's read-set, which is what makes a concurrent
// fence conflict the binding commit rather than racing it.
func txReadEpoch(tx interface{ Get([]byte) ([]byte, error) }, key []byte) (int64, error) {
	raw, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("read blob-ownership epoch: %w", err)
	}
	payload, serr := stripEnvelope(raw)
	if serr != nil {
		return 0, fmt.Errorf("decode blob-ownership epoch: %w", serr)
	}
	if len(payload) == 0 {
		return 0, nil
	}
	n, perr := strconv.ParseInt(string(payload), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("decode blob-ownership epoch %q: %w", payload, perr)
	}
	return n, nil
}
