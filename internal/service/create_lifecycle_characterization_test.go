package service

// Characterization of the create lifecycle as it behaves TODAY, pinned before
// the uniform-lifecycle refactor moves any of it.
//
// These are not aspirational. They record what a caller currently observes, so
// the refactor cannot change it silently. Whatever shape the create path ends
// up with, both observations below must survive:
//
//   - the transactional protocol commits READY with no pending window and
//     never flips status
//   - the detached protocol returns PENDING and reaches READY through a
//     background finalizer
//
// The detached half of that pair is covered in depth by
// upload_lifecycle_test.go. What is NOT covered anywhere on the default build
// is the TRANSACTIONAL half, because the only transactional adapter is behind
// -tags slatedb. That gap is why a refactor could collapse one protocol into
// the other and still look green, so it is closed here with a transactional
// double.

import (
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// statusSpyRepo records the status transitions the service asks for, so a test
// can assert a protocol never flips status rather than only that it ends READY.
type statusSpyRepo struct {
	PasteRepo
	markReady  int
	markFailed int
}

func (r *statusSpyRepo) MarkReady(s domain.Slug) error {
	r.markReady++
	return r.PasteRepo.MarkReady(s)
}

func (r *statusSpyRepo) MarkFailed(s domain.Slug) error {
	r.markFailed++
	return r.PasteRepo.MarkFailed(s)
}

// The bytes are bound in the same commit as the row, so the paste is READY the
// moment Create returns and no status flip ever happens.
func TestCreateLifecycle_Transactional_CommitsReadyWithNoFlip(t *testing.T) {
	repo := &statusSpyRepo{PasteRepo: storagetest.NewRepo(t)}
	u := NewUpload(repo, txTestBlobUnit{})
	t.Cleanup(u.WaitFinalize) // no background work should exist, but do not let one outlive the repo

	res, err := u.Create(strings.NewReader("# transactional create\n"), "key:tx-owner", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if res.Paste.Status != domain.PasteStatusReady {
		t.Fatalf("returned status %q; want %q, the transactional path has no pending window",
			res.Paste.Status, domain.PasteStatusReady)
	}
	if repo.markReady != 0 || repo.markFailed != 0 {
		t.Fatalf("status was flipped: MarkReady=%d MarkFailed=%d; want 0 and 0, the row commits ready",
			repo.markReady, repo.markFailed)
	}
}

// The persisted row agrees with what Create returned. Pinned separately because
// a refactor could get the return value right while committing something else.
func TestCreateLifecycle_Transactional_PersistsReady(t *testing.T) {
	repo := storagetest.NewRepo(t)
	u := NewUpload(repo, txTestBlobUnit{})
	t.Cleanup(u.WaitFinalize)

	res, err := u.Create(strings.NewReader("# persisted\n"), "key:tx-owner", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("persisted status %q; want %q", got.Status, domain.PasteStatusReady)
	}
}

// The two protocols currently differ in exactly one observable: the status a
// caller sees at Create. This pins the difference itself, so a refactor that
// makes status uniform has to change this test deliberately rather than by
// accident.
func TestCreateLifecycle_ProtocolsDifferOnlyInInitialStatus(t *testing.T) {
	txRepo := storagetest.NewRepo(t)
	txU := NewUpload(txRepo, txTestBlobUnit{})
	t.Cleanup(txU.WaitFinalize)
	txRes, err := txU.Create(strings.NewReader("# same input\n"), "key:pair-owner", "", "")
	if err != nil {
		t.Fatalf("transactional Create: %v", err)
	}

	blobs := newFakeBlobs()
	blobs.holdPut = make(chan struct{}) // park the finalizer so PENDING is observable
	detachedU, _, done := newStackWithBlobs(t, blobs)
	deRes, err := detachedU.Create(strings.NewReader("# same input\n"), "key:pair-owner", "", "")
	if err != nil {
		t.Fatalf("detached Create: %v", err)
	}

	if txRes.Paste.Status != domain.PasteStatusReady {
		t.Fatalf("transactional status %q; want ready", txRes.Paste.Status)
	}
	if deRes.Paste.Status != domain.PasteStatusPending {
		t.Fatalf("detached status %q; want pending", deRes.Paste.Status)
	}
	if txRes.Paste.Kind != deRes.Paste.Kind {
		t.Fatalf("kind diverges across protocols: %q vs %q; only status should differ",
			txRes.Paste.Kind, deRes.Paste.Kind)
	}

	close(blobs.holdPut)
	waitFinalize(t, done)
}
