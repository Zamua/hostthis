package celld

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestAppendRetriesWithOneOperationID(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path != "/paste/append" {
			t.Fatalf("path = %q, want /paste/append", req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"response-lost"}`))
			return
		}
		_, _ = w.Write([]byte(`{"appended":true,"ver":2,"wasPinned":false}`))
	}))
	defer server.Close()

	repo := NewPasteRepo(server.URL, server.Client())
	result, err := repo.AppendVersionWithQuotaCheck(
		context.Background(), "slugone1", "generation-1", domain.KindMarkdown, "sha-v2", 4, 10, time.Unix(8, 0),
	)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if result.NewVer != 2 {
		t.Fatalf("version = %d, want 2", result.NewVer)
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(bodies))
	}
	firstID, _ := bodies[0]["opId"].(string)
	secondID, _ := bodies[1]["opId"].(string)
	if firstID == "" || firstID != secondID {
		t.Fatalf("operation IDs = %q, %q; want one non-empty ID", firstID, secondID)
	}
	if bodies[0]["generation"] != "generation-1" || bodies[1]["generation"] != "generation-1" {
		t.Fatalf("generations = %#v, %#v; want generation-1", bodies[0]["generation"], bodies[1]["generation"])
	}
	if bodies[0]["userCap"] != float64(10) {
		t.Fatalf("userCap = %#v, want 10", bodies[0]["userCap"])
	}
}

func TestAppendMapsAtomicQuotaRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path != "/paste/append" {
			t.Fatalf("path = %q, want /paste/append", req.URL.Path)
		}
		w.WriteHeader(http.StatusInsufficientStorage)
		_, _ = w.Write([]byte(`{"error":"over-quota"}`))
	}))
	defer server.Close()

	repo := NewPasteRepo(server.URL, server.Client())
	_, err := repo.AppendVersionWithQuotaCheck(
		context.Background(), "slugone1", "generation-1", domain.KindHTML, "sha-v2", 4, 10, time.Unix(8, 0),
	)
	if err != domain.ErrOverUserQuota {
		t.Fatalf("err = %v, want ErrOverUserQuota", err)
	}
}

func TestOwnerListingSeparatesServedAndChargedBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/identity/list" {
			t.Fatalf("path = %q, want /identity/list", req.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"slug":"slugone1","status":"ready","kind":"html",
			"servedSize":2,"chargedSize":6,"at":7000,"updatedAt":8000,
			"latestVersion":2,"contentSha":"v1"
		}]`))
	}))
	defer server.Close()

	repo := NewPasteRepo(server.URL, server.Client())
	listed, err := repo.ListByOwner("owner")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("rows = %d, want 1", len(listed))
	}
	if listed[0].Size != 2 || listed[0].StoredBytes != 6 {
		t.Fatalf("size/stored = %d/%d, want 2/6", listed[0].Size, listed[0].StoredBytes)
	}
}
