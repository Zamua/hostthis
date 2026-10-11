package http

// testdata/read-vectors.json pins the read rules. celld/src/read.js answers the
// same vectors, so the Go read path and the Worker's cannot drift apart.

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	stdhttp "net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
)

type readVectorFile struct {
	Rows    map[string]json.RawMessage `json:"rows"`
	Vectors []readVector               `json:"vectors"`
}

type readVector struct {
	Name    string  `json:"name"`
	Row     *string `json:"row"`
	Request struct {
		Path  string `json:"path"`
		Raw   bool   `json:"raw"`
		Files bool   `json:"files"`
	} `json:"request"`
	Want json.RawMessage `json:"want"`
}

// vectorRow is the part of a stored row an answer echoes.
type vectorRow struct {
	Kind      string `json:"kind"`
	UpdatedAt int64  `json:"updatedAt"`
	UploadID  string `json:"uploadId"`
}

// servedEntryBlobs serves "bytes of <key>" and remembers the entry it was asked
// for, which is the answer's entry. An empty key is not found, as in
// storage.CompressedBlobStore.
type servedEntryBlobs struct{ read *domain.ManifestEntry }

func (b *servedEntryBlobs) Read(_ context.Context, e domain.ManifestEntry) (io.ReadCloser, int64, error) {
	b.read = &e
	if e.Key == "" {
		return nil, 0, domain.ErrNotFound
	}
	body := "bytes of " + e.Key
	return io.NopCloser(strings.NewReader(body)), int64(len(body)), nil
}

type roundTripFunc func(*stdhttp.Request) (*stdhttp.Response, error)

func (f roundTripFunc) RoundTrip(r *stdhttp.Request) (*stdhttp.Response, error) { return f(r) }

// storedRowCell answers the paste cell's get with row, or not found when row is
// nil.
func storedRowCell(row json.RawMessage) *stdhttp.Client {
	return &stdhttp.Client{Transport: roundTripFunc(func(r *stdhttp.Request) (*stdhttp.Response, error) {
		w := httptest.NewRecorder()
		if r.URL.Path == "/paste/get" && row != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(row)
		} else {
			stdhttp.Error(w, "not found", stdhttp.StatusNotFound)
		}
		return w.Result(), nil
	})}
}

func loadReadVectors(t *testing.T) readVectorFile {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/read-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f readVectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestReadVectors runs each vector's row through the celld adapter's decode, as
// the cell would serve it, and its request through serveSlug, then reads the
// answer back from the response.
func TestReadVectors(t *testing.T) {
	f := loadReadVectors(t)
	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			var row *vectorRow
			var stored json.RawMessage
			if v.Row != nil {
				var ok bool
				if stored, ok = f.Rows[*v.Row]; !ok {
					t.Fatalf("no row %q", *v.Row)
				}
				row = &vectorRow{}
				if err := json.Unmarshal(stored, row); err != nil {
					t.Fatal(err)
				}
			}
			blobs := &servedEntryBlobs{}
			srv := &Server{Pastes: celld.NewPasteRepo("https://cell", storedRowCell(stored)), Blobs: blobs}
			var query []string
			if v.Request.Raw {
				query = append(query, "raw=1")
			}
			if v.Request.Files {
				query = append(query, "files=1")
			}
			r := httptest.NewRequest(stdhttp.MethodGet, "/", nil)
			r.URL.RawQuery = strings.Join(query, "&")
			w := httptest.NewRecorder()
			srv.serveSlug(w, r, "abc23456", v.Request.Path)

			got := observeAnswer(t, row, v.Request.Path, w, blobs.read)
			var want any
			if err := json.Unmarshal(v.Want, &want); err != nil {
				t.Fatal(err)
			}
			if gotValue := normalizeJSON(t, got); !reflect.DeepEqual(gotValue, want) {
				gotJSON, _ := json.Marshal(gotValue)
				t.Fatalf("answer = %s\nwant     %s", gotJSON, v.Want)
			}
		})
	}
}

// observeAnswer reads the answer a response encodes. Where two actions serve
// identical responses (a markdown paste's ?raw and a knowledge base's root
// document), the row's kind names the action; everything else is evidence.
func observeAnswer(t *testing.T, row *vectorRow, path string, w *httptest.ResponseRecorder,
	read *domain.ManifestEntry,
) map[string]any {
	t.Helper()
	h := w.Header()
	body := w.Body.String()
	header := func(name, want string) {
		t.Helper()
		if got := h.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if row == nil {
		if w.Code != stdhttp.StatusNotFound || read != nil {
			t.Fatalf("status %d, blob read %v, want 404 and none", w.Code, read)
		}
		return map[string]any{"found": false}
	}
	served := func(action string, extra map[string]any) map[string]any {
		out := map[string]any{"action": action, "kind": row.Kind, "updatedAt": row.UpdatedAt, "uploadId": row.UploadID}
		maps.Copy(out, extra)
		return out
	}
	validated := func(etag string) {
		t.Helper()
		header("ETag", etag)
		header("Last-Modified", time.UnixMilli(row.UpdatedAt).UTC().Format(stdhttp.TimeFormat))
	}
	kind := domain.ContentKind(row.Kind)

	if read != nil {
		if w.Code != stdhttp.StatusOK || body != "bytes of "+read.Key {
			t.Fatalf("status %d body %q after reading %+v", w.Code, body, *read)
		}
		validated(`"` + read.Key + `"`)
		entry := map[string]any{"entry": map[string]any{
			"key": read.Key, "contentType": read.ContentType, "size": read.Size,
		}}
		switch {
		case kind == domain.KindHTML:
			header("Content-Type", "text/html; charset=utf-8")
			header("Cache-Control", contentCacheControl)
			return served("html", entry)
		case !kind.IsDirectory():
			header("Content-Type", rawContentType[kind])
			header("Cache-Control", contentCacheControl)
			return served("raw", entry)
		case kind == domain.KindKnowledgeBase && h.Get("Content-Type") == rawContentType[kind]:
			header("Cache-Control", contentCacheControl)
			return served("kbraw", entry)
		}
		ct := read.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		header("Content-Type", ct)
		header("Cache-Control", contentCacheControl)
		return served("file", entry)
	}

	switch {
	case w.Code == stdhttp.StatusNotFound:
		return served("notfound", nil)
	case w.Code == stdhttp.StatusGone:
		if body != failedPageHTML {
			t.Fatalf("410 body is not the failed page")
		}
		header("Cache-Control", "no-store")
		return map[string]any{"status": string(domain.PasteStatusFailed)}
	case w.Code == stdhttp.StatusOK && h.Get("Retry-After") == "1":
		if body != loadingPageHTML {
			t.Fatalf("pending body is not the loading page")
		}
		header("Cache-Control", "no-store")
		return map[string]any{"status": string(domain.PasteStatusPending)}
	case w.Code == stdhttp.StatusInternalServerError && body == "unsupported kind\n":
		// A shell answer naming a kind no template renders.
		if shellFor(kind) != nil {
			t.Fatalf("unsupported kind %q has a shell", kind)
		}
		return served("shell", map[string]any{"shell": row.Kind})
	case w.Code == stdhttp.StatusOK && h.Get("Content-Security-Policy") != "":
		for k, sh := range shells {
			if string(sh.html(k)) != body {
				continue
			}
			validated(`"` + sh.version + `"`)
			header("Content-Type", "text/html; charset=utf-8")
			header("Cache-Control", contentCacheControl)
			header("Content-Security-Policy", sh.policy())
			if k == domain.KindKnowledgeBase {
				return served("kbshell", map[string]any{"shell": string(k)})
			}
			return served("shell", map[string]any{"shell": string(k)})
		}
		t.Fatalf("shell body matches no shell")
	case w.Code == stdhttp.StatusOK && strings.HasPrefix(h.Get("ETag"), `"files-`):
		uploadID := strings.TrimSuffix(strings.TrimPrefix(h.Get("ETag"), `"files-`), `"`)
		validated(`"files-` + uploadID + `"`)
		header("Content-Type", "application/json; charset=utf-8")
		header("Cache-Control", contentCacheControl)
		var files []string
		if err := json.Unmarshal([]byte(body), &files); err != nil {
			t.Fatalf("file list: %v", err)
		}
		out := served("files", map[string]any{"files": files})
		out["uploadId"] = uploadID
		return out
	}
	t.Fatalf("unrecognized response: %d %v %q", w.Code, h, body)
	return nil
}

func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
