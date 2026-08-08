package storage

import (
	"errors"
	"testing"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// fakeTx answers the one method the guard uses.
type fakeTx struct {
	vals map[string][]byte
	err  error
}

func (f fakeTx) Get(key []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vals[string(key)]
	if !ok {
		return nil, backend.ErrNotFound
	}
	return v, nil
}
func (f fakeTx) Put([]byte, []byte) error { return nil }
func (f fakeTx) Delete([]byte) error      { return nil }

// The guard itself, over the outcomes the binding transaction can meet.
//
// Tested directly rather than through a write: without a blob plane an insert
// binds no refs, so the guard is never reached and a test driving one would
// pass no matter what the guard did.
func TestCheckBlobOwnership(t *testing.T) {
	slug := domain.Slug("guard123")
	key := string(shaleKeyBlobOwner(slug))

	for _, tc := range []struct {
		name    string
		tx      fakeTx
		epoch   int64
		wantErr error
	}{
		{
			name:  "matching epoch passes",
			tx:    fakeTx{vals: map[string][]byte{key: []byte("7")}},
			epoch: 7,
		},
		{
			name:    "stale epoch is fenced",
			tx:      fakeTx{vals: map[string][]byte{key: []byte("8")}},
			epoch:   7,
			wantErr: ErrFenced,
		},
		{
			name:    "epoch ahead of the record is fenced too",
			tx:      fakeTx{vals: map[string][]byte{key: []byte("3")}},
			epoch:   7,
			wantErr: ErrFenced,
		},
		{
			name:    "a vanished record fences rather than passing",
			tx:      fakeTx{vals: map[string][]byte{}},
			epoch:   7,
			wantErr: ErrFenced,
		},
		{
			name:  "no claim skips the check",
			tx:    fakeTx{vals: map[string][]byte{key: []byte("9")}},
			epoch: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBlobOwnership(tc.tx, slug, tc.epoch)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("got %v, want nil", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// An unreadable epoch must not read as "no claim". Failing open here would let
// a fenced writer bind precisely when the cluster is unhealthy.
func TestCheckBlobOwnership_UnreadableIsNotUnclaimed(t *testing.T) {
	boom := errors.New("shard unavailable")
	err := checkBlobOwnership(fakeTx{err: boom}, "guard456", 7)
	if err == nil {
		t.Fatal("an unreadable epoch passed the guard: a bind would proceed on a " +
			"claim nobody could verify")
	}
	if errors.Is(err, ErrFenced) {
		t.Fatalf("an unreadable epoch reported as FENCED (%v): that tells the caller "+
			"to abort permanently when it should retry", err)
	}
}
