package service

import (
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// incompleteVersionsRepo answers the owner gate and reports a truncated version
// list for every version lookup. The embedded nil PasteAdmin makes any other
// repo call panic.
type incompleteVersionsRepo struct {
	PasteAdmin
	paste domain.Paste
}

func (r *incompleteVersionsRepo) Get(domain.Slug) (domain.Paste, error) { return r.paste, nil }

func (r *incompleteVersionsRepo) GetVersion(domain.Slug, int) (domain.Version, error) {
	return domain.Version{}, domain.ErrVersionsIncomplete
}

// A version lookup that refused because the list is truncated must reach the
// caller as itself. Collapsing it into not-found - which is what every other
// GetVersion failure collapses to - would report a confident wrong answer about
// a version the service cannot see.
func TestVersionVerbs_IncompleteListIsNotReportedAsNotFound(t *testing.T) {
	owner := "key:abc"
	repo := &incompleteVersionsRepo{paste: domain.Paste{
		Slug: "inc00001", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		CreatedAt: time.Unix(1_700_000_000, 0),
	}}
	m := &Manage{Repo: repo, Now: time.Now}

	if _, err := m.Pin("inc00001", owner, 2); !errors.Is(err, domain.ErrVersionsIncomplete) {
		t.Fatalf("pin: got %v, want ErrVersionsIncomplete", err)
	}
	if _, err := m.DeleteVersion("inc00001", owner, 2); !errors.Is(err, domain.ErrVersionsIncomplete) {
		t.Fatalf("delete version: got %v, want ErrVersionsIncomplete", err)
	}
}
