package celld_test

// Release is membership, not arithmetic: replaying it against the same
// generation leaves the owner's charged total unchanged. The adapter's status
// guard stops a second delete before it reaches release, so only a direct
// replay, the shape a crash-recovering resolver produces, can pin this.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
)

func TestReleaseReplayDoesNotUnderCharge(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the release-replay probe")
	}
	repo := celld.NewPasteRepo(base, nil)
	owner := fmt.Sprintf("key:replay-%d", time.Now().UnixNano()%1000000)
	now := time.Now().UTC()

	mk := func(slug string, size int) domain.Paste {
		return domain.Paste{
			Slug: domain.Slug(slug), Identity: domain.Identity(owner),
			Status: domain.PasteStatusPending, Kind: domain.KindMarkdown,
			ContentSHA: "sha", Size: size, CreatedAt: now, UpdatedAt: now,
		}
	}
	doomed, survivor := mk("rp123456", 700), mk("rp223456", 300)
	for _, p := range []domain.Paste{doomed, survivor} {
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}

	stored, err := repo.Get(doomed.Slug)
	if err != nil {
		t.Fatalf("get %s: %v", doomed.Slug, err)
	}

	// Drive the cell's release directly, bypassing the adapter's status guard,
	// which is exactly what a replaying resolver does.
	release := func() {
		t.Helper()
		body := fmt.Sprintf(`{"slug":%q,"generation":%q}`, doomed.Slug.String(), stored.Generation)
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/identity/release?scope=%s", base, owner),
			strings.NewReader(body))
		if err != nil {
			t.Fatalf("build release: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("release: status %d", resp.StatusCode)
		}
	}

	release()
	once, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("sum after one release: %v", err)
	}
	if once != 300 {
		t.Fatalf("charged = %d after releasing the 700; want 300, the survivor", once)
	}

	release() // the replay
	twice, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("sum after the replay: %v", err)
	}
	if twice != once {
		t.Fatalf("charged = %d after a REPLAYED release, was %d. Release must be membership, "+
			"not arithmetic: a resolver only promises at-least-once.", twice, once)
	}
}
