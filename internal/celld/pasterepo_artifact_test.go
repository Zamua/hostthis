package celld

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestAppendRetriesWithOneOperationID(t *testing.T) {
	var attempts int
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		if c.Path != "/paste/append" {
			t.Fatalf("path = %q, want /paste/append", c.Path)
		}
		attempts++
		if attempts == 1 {
			return cellResponse(http.StatusBadGateway, `{"error":"response-lost"}`), nil
		}
		return cellResponse(http.StatusOK, `{"appended":true,"ver":2,"wasPinned":false}`), nil
	}}

	repo := NewPasteRepo("https://cell", f.client())
	result, err := repo.AppendVersionWithQuotaCheck(
		context.Background(), "slugone1", "generation-1", domain.KindMarkdown, "sha-v2", 4, 10, time.Unix(8, 0),
	)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if result.NewVer != 2 {
		t.Fatalf("version = %d, want 2", result.NewVer)
	}
	if len(f.calls) != 2 {
		t.Fatalf("attempts = %d, want 2", len(f.calls))
	}
	first, second := f.calls[0].fields(t), f.calls[1].fields(t)
	firstID, _ := first["opId"].(string)
	secondID, _ := second["opId"].(string)
	if firstID == "" || firstID != secondID {
		t.Fatalf("operation IDs = %q, %q; want one non-empty ID", firstID, secondID)
	}
	if first["generation"] != "generation-1" || second["generation"] != "generation-1" {
		t.Fatalf("generations = %#v, %#v; want generation-1", first["generation"], second["generation"])
	}
	if first["userCap"] != float64(10) {
		t.Fatalf("userCap = %#v, want 10", first["userCap"])
	}
}

func TestAppendMapsAtomicQuotaRefusal(t *testing.T) {
	f := fixedCell(http.StatusInsufficientStorage, `{"error":"over-quota"}`)
	repo := NewPasteRepo("https://cell", f.client())
	_, err := repo.AppendVersionWithQuotaCheck(
		context.Background(), "slugone1", "generation-1", domain.KindHTML, "sha-v2", 4, 10, time.Unix(8, 0),
	)
	if err != domain.ErrOverUserQuota {
		t.Fatalf("err = %v, want ErrOverUserQuota", err)
	}
	if f.last().Path != "/paste/append" {
		t.Fatalf("path = %q, want /paste/append", f.last().Path)
	}
}

func TestOwnerListingSeparatesServedAndChargedBytes(t *testing.T) {
	f := fixedCell(http.StatusOK, `[{
		"slug":"slugone1","status":"ready","kind":"html",
		"servedSize":2,"chargedSize":6,"at":7000,"updatedAt":8000,
		"latestVersion":2,"contentSha":"v1"
	}]`)
	repo := NewPasteRepo("https://cell", f.client())
	listed, err := repo.ListByOwner("owner")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if f.last().Path != "/identity/list" {
		t.Fatalf("path = %q, want /identity/list", f.last().Path)
	}
	if len(listed) != 1 {
		t.Fatalf("rows = %d, want 1", len(listed))
	}
	if listed[0].Size != 2 || listed[0].StoredBytes != 6 {
		t.Fatalf("size/stored = %d/%d, want 2/6", listed[0].Size, listed[0].StoredBytes)
	}
}

// A legacy row is addressed with an empty generation; the adapter must hand
// it to the cell, whose adoption path owns the outcome, rather than refuse
// locally with not-found.
func TestMutationsPassAnEmptyLegacyGenerationThrough(t *testing.T) {
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/paste/append":
			return cellResponse(http.StatusOK, `{"appended":true,"ver":2,"wasPinned":false}`), nil
		case "/paste/delversion":
			return cellResponse(http.StatusOK, `{"deleted":true}`), nil
		case "/paste/pin":
			return cellResponse(http.StatusOK, `{"pinned":true}`), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}

	repo := NewPasteRepo("https://cell", f.client())
	if _, err := repo.AppendVersionWithQuotaCheck(
		context.Background(), "legacy12", "", domain.KindMarkdown, "sha-v2", 4, 10, time.Unix(8, 0),
	); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := repo.DeleteVersion("legacy12", "", 1); err != nil {
		t.Fatalf("delete version: %v", err)
	}
	if err := repo.SetPinnedVersion("legacy12", "", domain.Version{VerNum: 1}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("paths = %d, want 3", len(f.calls))
	}
	for _, c := range f.calls {
		if generation := c.fields(t)["generation"]; generation != "" {
			t.Fatalf("%s sent generation %#v, want empty", c.Path, generation)
		}
	}
}
