package ssh_test

// A paste whose stored manifests fail to decode is not found to `get`, while
// its owner can still list its versions and delete it (docs/SPEC.md "Entries
// without an object key").

import (
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

// cellRowRepo answers the row reads and delete from the celld adapter, so the
// verbs meet that adapter's decoding of a stored row.
type cellRowRepo struct {
	*storage.MemRepo
	cell *celld.PasteRepo
}

func (r cellRowRepo) Get(s domain.Slug) (domain.Paste, error) { return r.cell.Get(s) }

func (r cellRowRepo) ListVersions(s domain.Slug) ([]domain.Version, error) {
	return r.cell.ListVersions(s)
}

func (r cellRowRepo) Delete(s domain.Slug, id domain.Identity, at time.Time) ([]string, error) {
	return r.cell.Delete(s, id, at)
}

func (r cellRowRepo) DropStaleOwnerEntry(s domain.Slug, owner string) (bool, error) {
	return r.cell.DropStaleOwnerEntry(s, owner)
}

func TestUndecodableManifest_GetNotFoundVersionsAndDeleteWork(t *testing.T) {
	slug := domain.NewRandomSlug()
	var owner atomic.Value
	owner.Store("")
	var removed atomic.Bool
	cell := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		switch r.URL.Path {
		case "/paste/get":
			_, _ = fmt.Fprintf(w, `{"slug":%q,"identity":%q,"generation":"generation-1","status":"ready",
				"kind":"html","size":3,"createdAt":7,"updatedAt":7,"manifest":"oops"}`, slug, owner.Load())
		case "/paste/versions":
			_, _ = fmt.Fprint(w, `[{"ver":2,"kind":"html","uploadId":"upload-2","size":3,"createdAt":7,"manifest":42}]`)
		case "/paste/remove":
			removed.Store(true)
			_, _ = fmt.Fprint(w, `{"removed":true,"uploads":[]}`)
		default:
			stdhttp.Error(w, "unexpected "+r.URL.Path, stdhttp.StatusInternalServerError)
		}
	}))
	t.Cleanup(cell.Close)
	repo := celld.NewPasteRepo(cell.URL, cell.Client())

	s := startStack(t, withManageRepo(func(m *storage.MemRepo) service.PasteAdmin {
		return cellRowRepo{MemRepo: m, cell: repo}
	}))
	owner.Store(domain.IdentityFromKeyFingerprint(s.keyedOwner).String())

	if _, stderr, exit := s.run("get "+slug.String(), nil); exit != hostssh.ExitNotFound {
		t.Fatalf("get exit = %d, want %d (stderr %q)", exit, hostssh.ExitNotFound, stderr)
	}
	stdout, stderr, exit := s.run("versions "+slug.String(), nil)
	if exit != hostssh.ExitOK || stdout == "" {
		t.Fatalf("versions = exit %d stdout %q, want the listing (stderr %q)", exit, stdout, stderr)
	}
	if _, stderr, exit := s.run("delete "+slug.String(), nil); exit != hostssh.ExitOK || !removed.Load() {
		t.Fatalf("delete exit = %d removed %v, want a removal (stderr %q)", exit, removed.Load(), stderr)
	}
}
