package service_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/service"
)

func TestDeleteVersion_FreesQuota(t *testing.T) {
	withSmallQuota(t, 10<<20)
	upload, manage, _ := newStack(t)
	owner := "key:dv-quota"

	// v1 = 3M, v2 = 6M -> 9M used (v2 serves, v1 idle)
	// v3 = 3M would total 12M -> ErrOverQuota
	// delete-version v1 frees 3M -> 6M, so v3 then fits at 9M
	r, err := upload.Create(bytes.NewReader(htmlBody(3_000_000)), owner, "", "")
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(6_000_000)), ""); err != nil {
		t.Fatalf("update v2: %v", err)
	}
	// Baseline: the v3 attempt fails before delete-version.
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(3_000_000)), ""); !errors.Is(err, service.ErrOverQuota) {
		t.Fatalf("expected over-quota baseline, got %v", err)
	}
	dr, err := manage.DeleteVersion(r.Paste.Slug, owner, 1)
	if err != nil {
		t.Fatalf("DeleteVersion v1: %v", err)
	}
	if dr.VerNum != 1 || dr.FreedBytes < 2_500_000 {
		t.Fatalf("delete result unexpected: %+v", dr)
	}
	// With v1 freed, v3 should now fit.
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(3_000_000)), ""); err != nil {
		t.Fatalf("after delete-version v1, 3M update should fit: %v", err)
	}
}

func TestDeleteVersion_RefusesCurrent(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dv-current"
	r, err := upload.Create(bytes.NewReader(htmlBody(1000)), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// v1 is the only version, so it is the served one.
	_, err = manage.DeleteVersion(r.Paste.Slug, owner, 1)
	if !errors.Is(err, service.ErrVersionCurrentlyServed) {
		t.Fatalf("expected ErrVersionCurrentlyServed, got %v", err)
	}
}

func TestDeleteVersion_RefusesPinnedCurrent(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dv-pinned"
	r, err := upload.Create(bytes.NewReader(htmlBody(1000)), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(1000)), ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Pinning makes v1 the served version even though v2 is newer.
	if _, err := manage.Pin(r.Paste.Slug, owner, 1); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := manage.DeleteVersion(r.Paste.Slug, owner, 1); !errors.Is(err, service.ErrVersionCurrentlyServed) {
		t.Fatalf("expected ErrVersionCurrentlyServed for pinned v1, got %v", err)
	}
	// v2 is not served (pin holds v1), so it should delete.
	if _, err := manage.DeleteVersion(r.Paste.Slug, owner, 2); err != nil {
		t.Fatalf("delete v2 (not served): %v", err)
	}
}

func TestDeleteVersion_AlreadyDeleted(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dv-already"
	r, err := upload.Create(bytes.NewReader(htmlBody(1000)), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(1000)), ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := manage.DeleteVersion(r.Paste.Slug, owner, 1); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if _, err := manage.DeleteVersion(r.Paste.Slug, owner, 1); !errors.Is(err, service.ErrVersionAlreadyDeleted) {
		t.Fatalf("expected ErrVersionAlreadyDeleted, got %v", err)
	}
}

func TestDeleteVersion_NotFound(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dv-notfound"
	r, err := upload.Create(bytes.NewReader(htmlBody(1000)), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manage.DeleteVersion(r.Paste.Slug, owner, 99); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for nonexistent ver, got %v", err)
	}
}

func TestDeleteVersion_VersionsListShowsTombstone(t *testing.T) {
	upload, manage, _ := newStack(t)
	owner := "key:dv-list"
	r, err := upload.Create(bytes.NewReader(htmlBody(1000)), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(1000)), ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := manage.DeleteVersion(r.Paste.Slug, owner, 1); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	vers, err := manage.Versions(r.Paste.Slug, owner)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	found := false
	for _, v := range vers {
		if v.VerNum == 1 {
			found = true
			if !v.Deleted {
				t.Fatalf("v1 should be flagged deleted, got %+v", v)
			}
		}
	}
	if !found {
		t.Fatalf("v1 should still be in versions list as tombstone, got %+v", vers)
	}
}
