package http_test

// A paste whose stored manifest fails to decode is not found, never a server
// error (docs/SPEC.md "Entries without an object key").

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
)

type countingBlobs struct{ reads int }

func (b *countingBlobs) Read(context.Context, domain.ManifestEntry) (io.ReadCloser, int64, error) {
	b.reads++
	return nil, 0, errors.New("no bytes")
}

func TestServe_UndecodableManifestIsNotFound(t *testing.T) {
	targets := map[string][]string{
		"html": {"/p/abc23456", "/p/abc23456?raw=1"},
		"site": {"/p/abc23456/", "/p/abc23456/index.html"},
	}
	for kind, paths := range targets {
		for name, manifest := range map[string]string{
			"string":      `"oops"`,
			"number":      `42`,
			"keyed entry": `{"Files":{"/":{"Key":"uploads/u1/0","Size":"3"}}}`,
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				cell := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
					if r.URL.Path != "/paste/get" {
						stdhttp.NotFound(w, r)
						return
					}
					_, _ = fmt.Fprintf(w, `{"slug":"abc23456","identity":"key:owner","generation":"generation-1",
						"status":"ready","kind":%q,"size":3,"createdAt":1,"updatedAt":1,"manifest":%s}`, kind, manifest)
				}))
				defer cell.Close()
				blobs := &countingBlobs{}
				srv := &httpapi.Server{Pastes: celld.NewPasteRepo(cell.URL, cell.Client()), Blobs: blobs}
				for _, target := range paths {
					w := httptest.NewRecorder()
					srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", target, nil))
					if w.Code != stdhttp.StatusNotFound || blobs.reads != 0 {
						t.Fatalf("GET %s = %d with %d reads, want 404 and none", target, w.Code, blobs.reads)
					}
				}
			})
		}
	}
}
