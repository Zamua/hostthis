package service

import (
	"bytes"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// fiveVersions uploads v1..v5 of one paste and returns its slug.
func fiveVersions(t *testing.T, upload *Upload, manage *Manage, owner string) domain.Slug {
	t.Helper()
	r, err := upload.Create(bytes.NewReader(htmlBody(1000)), ownerOf(owner), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 2; i <= 5; i++ {
		if _, err := manage.Update(r.Paste.Slug, ownerOf(owner), bytes.NewReader(htmlBody(1000+i)), ""); err != nil {
			t.Fatalf("update v%d: %v", i, err)
		}
	}
	return r.Paste.Slug
}

func live(t *testing.T, manage *Manage, slug domain.Slug, owner string) []int {
	t.Helper()
	vers, err := manage.Versions(slug, ownerOf(owner))
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	out := []int{}
	for _, v := range vers {
		if !v.Deleted {
			out = append(out, v.VerNum)
		}
	}
	return out
}

func pruneKeep(keep int) VersionPicker {
	return func(vers []domain.Version, served int) []domain.Version {
		return domain.VersionsToPrune(vers, served, keep)
	}
}

func TestDeleteVersions_DryRunDeletesNothing(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dvs-dry"
	slug := fiveVersions(t, upload, manage, owner)
	res, err := manage.DeleteVersions(slug, ownerOf(owner), pruneKeep(1), true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(res.Picked) != 3 || len(res.Deleted) != 0 || res.FreedBytes != 0 {
		t.Fatalf("dry run result: %+v", res)
	}
	if got := live(t, manage, slug, owner); len(got) != 5 {
		t.Fatalf("dry run deleted versions: live %v", got)
	}
}

func TestDeleteVersions_PruneKeepsServedAndNewest(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dvs-prune"
	slug := fiveVersions(t, upload, manage, owner)
	// Pin v2: it must survive alongside the newest kept one (v5).
	if _, err := manage.Pin(slug, ownerOf(owner), 2); err != nil {
		t.Fatalf("pin: %v", err)
	}
	res, err := manage.DeleteVersions(slug, ownerOf(owner), pruneKeep(1), false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(res.Deleted) != 3 || res.FreedBytes <= 0 {
		t.Fatalf("prune result: %+v", res)
	}
	got := live(t, manage, slug, owner)
	if len(got) != 2 || got[0] != 5 || got[1] != 2 {
		t.Fatalf("live after prune = %v, want [5 2]", got)
	}
}

func TestDeleteVersions_RangeSkipsServed(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dvs-range"
	slug := fiveVersions(t, upload, manage, owner)
	pick := func(vers []domain.Version, served int) []domain.Version {
		return domain.VersionsInRange(vers, served, 1, 5)
	}
	res, err := manage.DeleteVersions(slug, ownerOf(owner), pick, false)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(res.Deleted) != 4 {
		t.Fatalf("range deleted %d, want 4", len(res.Deleted))
	}
	if got := live(t, manage, slug, owner); len(got) != 1 || got[0] != 5 {
		t.Fatalf("live after range = %v, want [5]", got)
	}
}

func TestDeleteVersions_NotOwner(t *testing.T) {
	upload, manage, _ := newStack(t)
	slug := fiveVersions(t, upload, manage, "key:dvs-owner")
	if _, err := manage.DeleteVersions(slug, ownerOf("key:dvs-other"), pruneKeep(0), false); err != ErrNotFound {
		t.Fatalf("other owner: %v, want ErrNotFound", err)
	}
}
