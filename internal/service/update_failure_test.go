package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
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
		{"another change settling", domain.ErrBusy, domain.ErrBusy, false},
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

// appendAnswerCell serves an owned row and answers every append with one
// scripted status and body.
type appendAnswerCell struct {
	status int
	body   string
}

func (c appendAnswerCell) RoundTrip(req *http.Request) (*http.Response, error) {
	status, body := http.StatusOK, `{"slug":"slugone1","identity":"key:owner","generation":"generation-1","status":"ready","kind":"html"}`
	if req.URL.Path == "/paste/append" {
		status, body = c.status, c.body
	}
	return &http.Response{
		StatusCode: status, Header: make(http.Header), Request: req,
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

// Over the celld adapter only an explicit absent answer deletes the staged
// object; an answer the adapter cannot identify keeps it.
func TestUpdate_CelldAppendAnswerDecidesTheObjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantKept bool
	}{
		{"explicit absent", http.StatusOK, `{"appended":false,"reason":"absent"}`, false},
		{"another operation pending", http.StatusLocked, `{"error":"artifact-operation-pending"}`, false},
		{"unknown op", http.StatusNotFound, "unknown op\n", true},
		{"accounting conflict", http.StatusConflict, `{"error":"artifact-accounting-conflict"}`, true},
		{"empty server error", http.StatusInternalServerError, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blobs, root := realBlobsAt(t)
			repo := celld.NewPasteRepo("https://cell", &http.Client{Transport: appendAnswerCell{tc.status, tc.body}})
			m := NewManage(repo, NewStandaloneBlobUnit(blobs))
			if _, err := m.Update("slugone1", "key:owner", strings.NewReader("<!doctype html><p>v2</p>"), ""); err == nil {
				t.Fatal("update = nil, want the append's failure")
			}
			want := 0
			if tc.wantKept {
				want = 1
			}
			if n := objectsUnder(t, root); n != want {
				t.Fatalf("objects after refused update = %d, want %d", n, want)
			}
		})
	}
}
