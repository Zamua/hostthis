package storage_test

// The per-version delete's served-version guard over a document nothing can
// account for. The guard runs in the service, the document lives here, so the
// test drives the real service against the real repo.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// dropVersionFromDoc removes one record from the stored document, the shape a
// heal that could not read that row leaves behind.
func dropVersionFromDoc(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, ver int) {
	t.Helper()
	key := storage.VersionsDocKeyForTest(slug)
	raw, err := repo.GetRawForTest(key)
	if err != nil || len(raw) == 0 {
		t.Fatalf("read versions doc: err=%v len=%d", err, len(raw))
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode versions doc: %v", err)
	}
	held, _ := doc["versions"].([]any)
	kept := make([]any, 0, len(held))
	for _, entry := range held {
		rec, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("unexpected version record shape %T", entry)
		}
		if n, ok := rec["ver_num"].(float64); ok && int(n) == ver {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == len(held) {
		t.Fatalf("fixture: v%d was not in the document", ver)
	}
	doc["versions"] = kept
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode versions doc: %v", err)
	}
	if err := repo.PutRawForTest(key, out); err != nil {
		t.Fatalf("write versions doc: %v", err)
	}
}

// A guard derived from the version LIST and a target read through the gated
// path disagree on an unaccountable document: the list still answers from it,
// the target already fell back to the rows. The owner then tombstones the
// version the URL serves and unbinds the bytes it resolves. The guard reads the
// HEAD instead, which is the thing that serves.
func TestVersionsDoc_DeleteRefusesTheServedVersionOverAnUnaccountableDocument(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vguard01")
	owner := "key:vguard"
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-vguard-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vguard-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.PinnedVersion != 0 || head.ContentSHA != "sha-vguard-v3" {
		t.Fatalf("fixture: the head must serve v3 unpinned, got pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}

	// What a binary predating the document leaves: a document short of a
	// version, over a head whose generation that binary's re-marshal dropped.
	dropVersionFromDoc(t, repo, slug, 3)
	stripHeadVersionsGen(t, repo, slug)

	// Fixture: the ungated list names v2 as the newest live version, while the
	// rows the gated read falls back to still hold a live v3.
	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if got := verNums(vers); len(got) == 0 || got[0] != 2 {
		t.Fatalf("fixture: the document must claim v2 is newest, got %v", got)
	}
	if v3, gerr := repo.GetVersion(slug, 3); gerr != nil || v3.Deleted || v3.ContentSHA != head.ContentSHA {
		t.Fatalf("fixture: v3 must be live and carry the head's descriptor: %+v err=%v", v3, gerr)
	}

	manage := &service.Manage{Repo: repo, Now: func() time.Time { return now }}
	if _, err := manage.DeleteVersion(slug, owner, 3); !errors.Is(err, service.ErrVersionCurrentlyServed) {
		t.Fatalf("tombstoning the version the URL serves must be refused, got %v", err)
	}
	after, err := repo.GetVersion(slug, 3)
	if err != nil || after.Deleted {
		t.Fatalf("the served version must survive the refused delete: %+v err=%v", after, err)
	}
	// A version the head does NOT serve still frees, so the guard refuses on
	// what is served rather than on the damage.
	if _, err := manage.DeleteVersion(slug, owner, 1); err != nil {
		t.Fatalf("an unserved version must still free over the same document: %v", err)
	}
}
