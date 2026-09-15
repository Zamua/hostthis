package celld

// The upload-id wire contract with the Worker: ids travel on create, append and
// the versions list, and each delete's answer names the uploads it removed.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// rootKey digs the document root's object key out of a decoded manifest.
func rootKey(t *testing.T, manifest any) any {
	t.Helper()
	m, _ := manifest.(map[string]any)
	files, _ := m["Files"].(map[string]any)
	root, _ := files[domain.Root].(map[string]any)
	return root["Key"]
}

func TestInsertRowCarriesUploadID(t *testing.T) {
	var row map[string]any
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/identity/reserve":
			return cellResponse(http.StatusOK, ""), nil
		case "/paste/put":
			row, _ = c.fields(t)["row"].(map[string]any)
			return cellResponse(http.StatusNoContent, ""), nil
		case "/identity/confirm":
			return cellResponse(http.StatusNoContent, ""), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}
	p := createPaste()
	p.ContentSHA = ""
	p.UploadID = "upload-1"
	p.Manifest = domain.DocumentManifest(domain.ManifestEntry{Key: domain.UploadObjectKey("upload-1", 0)})

	repo := NewPasteRepo("https://cell", f.client())
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 10, time.Now()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row["uploadId"] != "upload-1" {
		t.Fatalf("put row uploadId = %#v, want upload-1", row["uploadId"])
	}
	if got := rootKey(t, row["manifest"]); got != "uploads/upload-1/0" {
		t.Fatalf("put row root key = %#v, want uploads/upload-1/0", got)
	}
}

func TestAppendCarriesUploadID(t *testing.T) {
	f := fixedCell(http.StatusOK, `{"appended":true,"ver":2,"wasPinned":false}`)
	repo := NewPasteRepo("https://cell", f.client())
	manifest := domain.DocumentManifest(domain.ManifestEntry{Key: "uploads/upload-2/0"})
	if _, err := repo.AppendVersionWithQuotaCheck(
		context.Background(), "slugone1", "generation-1", domain.KindHTML, "upload-2", manifest, 4, 10, time.Unix(8, 0),
	); err != nil {
		t.Fatalf("append: %v", err)
	}
	body := f.last().fields(t)
	if body["uploadId"] != "upload-2" || body["contentSha"] != "" {
		t.Fatalf("append uploadId/contentSha = %#v/%#v, want upload-2 and empty", body["uploadId"], body["contentSha"])
	}
	if got := rootKey(t, body["manifest"]); got != "uploads/upload-2/0" {
		t.Fatalf("append root key = %#v, want uploads/upload-2/0", got)
	}
}

func TestListVersionsReadsUploadIDs(t *testing.T) {
	f := fixedCell(http.StatusOK, `[
		{"ver":2,"kind":"html","uploadId":"upload-2","size":4,"createdAt":1},
		{"ver":1,"kind":"html","contentSha":"abc","size":3,"createdAt":1}
	]`)
	repo := NewPasteRepo("https://cell", f.client())
	vers, err := repo.ListVersions("slugone1")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(vers) != 2 || vers[0].UploadID != "upload-2" || vers[1].UploadID != "" || vers[1].ContentSHA != "abc" {
		t.Fatalf("versions = %+v, want v2 with upload-2 and a legacy v1", vers)
	}
}

// Stored rows may still carry content shas. They decode, and name no bytes.
func TestGetDecodesRowsCarryingContentSHAs(t *testing.T) {
	for name, body := range map[string]string{
		"row without a manifest": `{"slug":"slugone1","generation":"generation-1","status":"ready",
			"kind":"html","contentSha":"abc","size":3}`,
		"entry holding only a sha": `{"slug":"slugone1","generation":"generation-1","status":"ready",
			"kind":"html","contentSha":"abc","size":3,
			"manifest":{"Files":{"/":{"SHA":"abc","Size":3,"CompressedSize":2,"ContentType":"","Kind":"html"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo := NewPasteRepo("https://cell", fixedCell(http.StatusOK, body).client())
			p, err := repo.Get("slugone1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if p.Size != 3 || p.Kind != domain.KindHTML {
				t.Fatalf("paste = %+v, want its metadata intact", p)
			}
			if key := p.RootEntry().Key; key != "" {
				t.Fatalf("root key = %q, want none", key)
			}
		})
	}
}

func removeCell(t *testing.T, removeAnswer string) *fakeCell {
	return &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/paste/get":
			return cellResponse(http.StatusOK, `{"slug":"slugone1","generation":"generation-1"}`), nil
		case "/paste/remove":
			return cellResponse(http.StatusOK, removeAnswer), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}
}

func TestDeleteReturnsTheRemovedUploads(t *testing.T) {
	f := removeCell(t, `{"removed":true,"uploads":["upload-1","","upload-3"]}`)
	repo := NewPasteRepo("https://cell", f.client())
	ids, err := repo.Delete("slugone1", "key:owner", time.UnixMilli(7))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if want := []string{"upload-1", "upload-3"}; !slices.Equal(ids, want) {
		t.Fatalf("uploads = %v, want %v", ids, want)
	}
}

func TestDeleteRefusalNamesNoUploads(t *testing.T) {
	f := removeCell(t, `{"removed":false,"reason":"not-owner","uploads":["upload-1"]}`)
	repo := NewPasteRepo("https://cell", f.client())
	ids, err := repo.Delete("slugone1", "key:someone-else", time.UnixMilli(7))
	if !errors.Is(err, domain.ErrNotFound) || ids != nil {
		t.Fatalf("refused delete = (%v, %v), want no uploads and ErrNotFound", ids, err)
	}
}

func TestDeleteVersionReturnsTheUpload(t *testing.T) {
	for answer, want := range map[string]string{
		`{"deleted":true,"totalSize":3,"upload":"upload-2"}`: "upload-2",
		`{"deleted":true,"totalSize":3}`:                     "",
	} {
		f := fixedCell(http.StatusOK, answer)
		repo := NewPasteRepo("https://cell", f.client())
		got, err := repo.DeleteVersion("slugone1", "generation-1", 2)
		if err != nil || got != want {
			t.Fatalf("DeleteVersion with %s = (%q, %v), want %q", answer, got, err, want)
		}
	}
}
