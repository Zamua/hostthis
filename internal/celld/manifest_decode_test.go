package celld

// A stored manifest of the wrong shape reads as no manifest and never fails its
// record (docs/SPEC.md "Entries without an object key").

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

var undecodableManifests = map[string]string{
	"string":             `"oops"`,
	"number":             `42`,
	"boolean":            `true`,
	"array":              `[{"Key":"uploads/u1/0"}]`,
	"files not a map":    `{"Files":"x"}`,
	"entry field type":   `{"Files":{"/":{"Size":"3"}}}`,
	"keyed mistyped row": `{"Files":{"/":{"Key":"uploads/u1/0","Size":"3"}}}`,
}

func rowWithManifest(manifest string) string {
	return fmt.Sprintf(`{"slug":"slugone1","identity":"key:owner","generation":"generation-1",
		"status":"ready","kind":"html","uploadId":"upload-1","size":3,"pinnedVersion":0,
		"createdAt":7,"updatedAt":9,"manifest":%s}`, manifest)
}

func loggedRepo(f *fakeCell) (*PasteRepo, *bytes.Buffer) {
	var buf bytes.Buffer
	repo := NewPasteRepo("https://cell", f.client())
	repo.Logger = log.New(&buf, "", 0)
	return repo, &buf
}

func logLines(buf *bytes.Buffer) []string {
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

func TestGetReadsAnUndecodableManifestAsNone(t *testing.T) {
	for name, manifest := range undecodableManifests {
		t.Run(name, func(t *testing.T) {
			repo, logged := loggedRepo(fixedCell(http.StatusOK, rowWithManifest(manifest)))
			p, err := repo.Get("slugone1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if p.Slug != "slugone1" || p.Identity != "key:owner" || p.Generation != "generation-1" ||
				p.Status != domain.PasteStatusReady || p.Kind != domain.KindHTML || p.Size != 3 ||
				p.UploadID != "upload-1" || !p.CreatedAt.Equal(time.UnixMilli(7)) {
				t.Fatalf("paste = %+v, want its metadata intact", p)
			}
			if len(p.Manifest.Files) != 0 || p.RootEntry().Key != "" {
				t.Fatalf("manifest = %+v, want none", p.Manifest)
			}
			if lines := logLines(logged); len(lines) != 1 || !strings.Contains(lines[0], "slugone1") {
				t.Fatalf("log = %q, want one skip naming the slug", logged.String())
			}
		})
	}
}

func TestGetLogsNothingForAWellFormedOrAbsentManifest(t *testing.T) {
	for name, manifest := range map[string]string{
		"null":  `null`,
		"keyed": `{"Files":{"/":{"Key":"uploads/u1/0","Size":3}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo, logged := loggedRepo(fixedCell(http.StatusOK, rowWithManifest(manifest)))
			p, err := repo.Get("slugone1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if name == "keyed" && p.RootEntry().Key != "uploads/u1/0" {
				t.Fatalf("root = %+v, want the stored key", p.RootEntry())
			}
			if logged.Len() != 0 {
				t.Fatalf("log = %q, want nothing", logged.String())
			}
		})
	}
}

func TestGetStillRefusesAnUndecodableRowField(t *testing.T) {
	body := `{"slug":"slugone1","generation":"generation-1","status":"ready","size":"big",
		"manifest":{"Files":{"/":{"Key":"uploads/u1/0","Size":3}}}}`
	repo := NewPasteRepo("https://cell", fixedCell(http.StatusOK, body).client())
	if _, err := repo.Get("slugone1"); err == nil {
		t.Fatal("get of a row with a mistyped size succeeded, want a decode error")
	}
}

func TestListVersionsReadsAnUndecodableManifestAsNone(t *testing.T) {
	for name, manifest := range undecodableManifests {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`[
				{"ver":3,"kind":"html","uploadId":"upload-3","size":5,"createdAt":1,"manifest":%[1]s},
				{"ver":2,"kind":"html","uploadId":"upload-2","size":4,"createdAt":1,
					"manifest":{"Files":{"/":{"Key":"uploads/upload-2/0","Size":4}}}},
				{"ver":1,"kind":"html","uploadId":"upload-1","size":3,"createdAt":1,"deleted":true,"manifest":%[1]s}
			]`, manifest)
			repo, logged := loggedRepo(fixedCell(http.StatusOK, body))
			vers, err := repo.ListVersions("slugone1")
			if err != nil {
				t.Fatalf("list versions: %v", err)
			}
			if len(vers) != 3 {
				t.Fatalf("versions = %+v, want all three", vers)
			}
			for _, i := range []int{0, 2} {
				if v := vers[i]; len(v.Manifest.Files) != 0 || v.UploadID == "" || v.Size == 0 {
					t.Fatalf("v%d = %+v, want its metadata and no manifest", v.VerNum, v)
				}
			}
			if !vers[2].Deleted {
				t.Fatalf("v1 = %+v, want its tombstone kept", vers[2])
			}
			if root := vers[1].Manifest.Files[domain.Root]; root.Key != "uploads/upload-2/0" {
				t.Fatalf("v2 root = %+v, want its stored key", root)
			}
			lines := logLines(logged)
			if len(lines) != 2 {
				t.Fatalf("log = %q, want one skip per undecodable version", logged.String())
			}
			for _, line := range lines {
				if !strings.Contains(line, "slugone1") {
					t.Fatalf("log line %q does not name the slug", line)
				}
			}
		})
	}
}

func TestListVersionsStillRefusesAnUndecodableVersionField(t *testing.T) {
	body := `[{"ver":"two","kind":"html","size":4,"createdAt":1}]`
	repo := NewPasteRepo("https://cell", fixedCell(http.StatusOK, body).client())
	if _, err := repo.ListVersions("slugone1"); err == nil {
		t.Fatal("list of a version with a mistyped ver succeeded, want a decode error")
	}
}

// Every adapter path that reads the row or its versions keeps working for a
// paste whose stored manifests are undecodable.
func TestRowReadersTolerateAnUndecodableManifest(t *testing.T) {
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/paste/get":
			return cellResponse(http.StatusOK, rowWithManifest(`"oops"`)), nil
		case "/paste/versions":
			return cellResponse(http.StatusOK, `[
				{"ver":2,"kind":"html","uploadId":"upload-2","size":4,"createdAt":1,"manifest":42}
			]`), nil
		case "/paste/remove":
			return cellResponse(http.StatusOK, `{"removed":true,"uploads":["upload-1","upload-2"]}`), nil
		}
		t.Errorf("unexpected path %q", c.Path)
		return cellResponse(http.StatusInternalServerError, ""), nil
	}}
	repo := NewPasteRepo("https://cell", f.client())

	if dropped, err := repo.DropStaleOwnerEntry("slugone1", "key:owner"); err != nil || dropped {
		t.Fatalf("DropStaleOwnerEntry = (%v, %v), want the paste treated as present", dropped, err)
	}
	if served, err := repo.IsVersionServed("slugone1", 2); err != nil || !served {
		t.Fatalf("IsVersionServed(2) = (%v, %v), want the latest served", served, err)
	}
	if v, err := repo.GetVersion("slugone1", 2); err != nil || v.UploadID != "upload-2" {
		t.Fatalf("GetVersion(2) = (%+v, %v), want its metadata", v, err)
	}
	ids, err := repo.Delete("slugone1", "key:owner", time.UnixMilli(7))
	if err != nil || len(ids) != 2 {
		t.Fatalf("Delete = (%v, %v), want both uploads", ids, err)
	}
}
