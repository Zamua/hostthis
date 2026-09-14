package service

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// scriptedAppendRepo is a real repo whose append answers with a scripted error.
type scriptedAppendRepo struct {
	*storage.MemRepo
	appendErr error
}

func (r scriptedAppendRepo) AppendVersionWithQuotaCheck(context.Context, domain.Slug, string, domain.ContentKind,
	string, domain.Manifest, int, int64, time.Time,
) (domain.AppendResult, error) {
	return domain.AppendResult{}, r.appendErr
}

// An update whose append is refused deletes the object it staged; one whose
// outcome is unknown keeps it, because the append may still publish.
func TestUpdate_FailedAppendObjects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		appendErr error
		wantErr   error
		wantKept  bool
	}{
		{"over quota", domain.ErrOverUserQuota, ErrOverQuota, false},
		{"not found", domain.ErrNotFound, domain.ErrNotFound, false},
		{"outcome unknown", errors.New("celld: /paste/append: connection reset"), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := storagetest.NewRepo(t)
			blobs, root := realBlobsAt(t)
			up := NewUpload(repo, NewStandaloneBlobUnit(blobs))
			t.Cleanup(up.WaitFinalize)
			res, err := up.Create(bytes.NewReader([]byte("<!doctype html><p>v1</p>")), "key:owner", "", "")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			up.WaitFinalize()

			m := NewManage(scriptedAppendRepo{MemRepo: repo, appendErr: tc.appendErr}, NewStandaloneBlobUnit(blobs))
			_, err = m.Update(res.Paste.Slug, "key:owner", bytes.NewReader([]byte("<!doctype html><p>v2</p>")), "")
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("update = %v, want %v", err, tc.wantErr)
			}
			want := 1
			if tc.wantKept {
				want = 2
			}
			if n := objectsUnder(t, root); n != want {
				t.Fatalf("objects after failed update = %d, want %d", n, want)
			}
		})
	}
}
