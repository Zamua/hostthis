package worker

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/workertest"
)

func TestDenylistRepo_ReadsWhatTheOperatorAdded(t *testing.T) {
	base := workertest.Endpoint(t)
	repo := NewDenylistRepo(base, nil)
	got, err := repo.Denylist(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v, %v", got, err)
	}
	key := "SHA256:" + strings.Repeat("ab", 32)
	for _, body := range []string{
		`{"entry":"` + key + `","reason":"abuse"}`,
		`{"entry":"203.0.113.9/24","reason":""}`,
	} {
		res, err := http.Post(base+"/denylist/add", "application/json", bytes.NewBufferString(body))
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("add %s: %v %v", body, res, err)
		}
		_ = res.Body.Close()
	}
	got, err = repo.Denylist(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[domain.DenyEntry]bool{{Entry: key, Reason: "abuse"}: true, {Entry: "203.0.113.0/24"}: true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("Denylist = %v, want %v", got, want)
	}
}
