package service

import (
	"bytes"
	"errors"
	"testing"
)

func TestQuota_BlocksOversum(t *testing.T) {
	withSmallQuota(t, 1<<20)
	upload, _, _ := newStack(t)
	owner := "key:test-id"

	if _, err := upload.Create(bytes.NewReader(htmlBody(600_000)), owner, "", ""); err != nil {
		t.Fatalf("first 600K upload: %v", err)
	}
	if _, err := upload.Create(bytes.NewReader(htmlBody(500_000)), owner, "", ""); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("second 500K should be over quota, got %v", err)
	}
}

func TestQuota_FreedByDelete(t *testing.T) {
	withSmallQuota(t, 1<<20)
	upload, manage, _ := newStack(t)
	owner := "key:test-id"

	r1, err := upload.Create(bytes.NewReader(htmlBody(900_000)), owner, "", "")
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// At 900K used, a 300K upload would exceed.
	if _, err := upload.Create(bytes.NewReader(htmlBody(300_000)), owner, "", ""); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("should be over quota before delete, got %v", err)
	}
	// Delete frees the 900K.
	if err := manage.Delete(r1.Paste.Slug, owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := upload.Create(bytes.NewReader(htmlBody(300_000)), owner, "", ""); err != nil {
		t.Fatalf("after delete, 300K should fit: %v", err)
	}
}

func TestQuota_VersionsCount(t *testing.T) {
	withSmallQuota(t, 1<<20)
	upload, manage, _ := newStack(t)
	owner := "key:test-id"

	// Every version row counts toward the identity's active bytes, so a 600K
	// paste updated with another 600K totals 1.2M and breaches a 1 MiB cap.
	r, err := upload.Create(bytes.NewReader(htmlBody(600_000)), owner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manage.Update(r.Paste.Slug, owner, bytes.NewReader(htmlBody(600_000)), ""); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("second 600K version should be over quota, got %v", err)
	}
}

func TestQuota_PerIdentityIndependent(t *testing.T) {
	withSmallQuota(t, 1<<20)
	upload, _, _ := newStack(t)

	if _, err := upload.Create(bytes.NewReader(htmlBody(900_000)), "key:alice", "", ""); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := upload.Create(bytes.NewReader(htmlBody(900_000)), "key:bob", "", ""); err != nil {
		t.Fatalf("bob: %v", err)
	}
	// Alice can't upload more without freeing.
	if _, err := upload.Create(bytes.NewReader(htmlBody(200_000)), "key:alice", "", ""); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("alice second should be over quota, got %v", err)
	}
}
