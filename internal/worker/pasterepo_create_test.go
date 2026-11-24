package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func createPaste() domain.Paste {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	return domain.Paste{
		Slug: "create23", Generation: "generation-1", Identity: "key:owner",
		Status: domain.PasteStatusPending, Kind: domain.KindHTML,
		Size: 3, CreatedAt: at, UpdatedAt: at,
	}
}

// A create is one request to the publish route, carrying the row, its
// fingerprint and the intent naming this slug and generation.
func TestPublishIsOneRequestCarryingTheWholeCreate(t *testing.T) {
	f := fixedCell(http.StatusOK, `{"active":42}`)
	repo := NewPasteRepo("https://cell", f.client())
	active, err := repo.Publish(context.Background(), createPaste(), 10, time.UnixMilli(7), "")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if active != 42 {
		t.Fatalf("owner bytes = %d, want the 42 the route answered", active)
	}
	if len(f.calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.calls))
	}
	c := f.last()
	if c.Method != http.MethodPost || c.Path != "/publish/paste" {
		t.Fatalf("request = %s %s, want POST /publish/paste", c.Method, c.Path)
	}
	body := c.fields(t)
	row, _ := body["row"].(map[string]any)
	if row["slug"] != "create23" || row["identity"] != "key:owner" || row["generation"] != "generation-1" {
		t.Fatalf("row = %v", row)
	}
	if fp, _ := body["fingerprint"].(string); fp == "" {
		t.Fatal("no fingerprint")
	}
	if body["intentId"] != "create:create23:generation-1" || body["userCap"] != float64(10) || body["now"] != float64(7) {
		t.Fatalf("body = %v", body)
	}
	if pod, ok := body["pod"]; !ok || pod != "" {
		t.Fatalf("pod = %v (present %v), want the empty link checked", pod, ok)
	}
}

// A deploy's create, through InsertWithQuotaCheck, names no pod link, so none
// is checked.
func TestInsertWithQuotaCheckChecksNoPod(t *testing.T) {
	f := fixedCell(http.StatusOK, `{"active":3}`)
	repo := NewPasteRepo("https://cell", f.client())
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, ok := f.last().fields(t)["pod"]; ok {
		t.Fatal("an insert named a pod link")
	}
}

// Each refusal the route answers maps to the sentinel the services act on.
func TestPublishRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   func(error) bool
	}{
		{"over quota", http.StatusInsufficientStorage, `{"error":"over-quota"}`,
			func(err error) bool { return errors.Is(err, domain.ErrOverUserQuota) }},
		{"slug taken", http.StatusConflict, `{"error":"slug-taken"}`,
			func(err error) bool { return errors.Is(err, domain.ErrSlugTaken) }},
		{"create aborted", http.StatusConflict, `{"error":"create-aborted"}`,
			func(err error) bool { return errors.Is(err, domain.ErrSlugTaken) }},
		{"too large", http.StatusRequestEntityTooLarge, `{"error":"version-too-large"}`,
			func(err error) bool { return errors.Is(err, domain.ErrTooManyFiles) }},
		{"pod moved", http.StatusConflict, `{"error":"pod-moved","pod":"0123456789abcdef"}`,
			func(err error) bool {
				moved, ok := errors.AsType[*domain.PodMovedError](err)
				return ok && moved.Link == "0123456789abcdef"
			}},
		{"another conflict", http.StatusConflict, `{"error":"create-mismatch"}`,
			func(err error) bool { return err != nil && !errors.Is(err, domain.ErrSlugTaken) }},
		{"confirm refused", http.StatusBadGateway, `{"error":"confirm-refused"}`,
			func(err error) bool { return err != nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := NewPasteRepo("https://cell", fixedCell(tc.status, tc.body).client())
			_, err := repo.Publish(context.Background(), createPaste(), 10, time.Now(), "")
			if !tc.want(err) {
				t.Fatalf("publish error = %v", err)
			}
		})
	}
}

// A lost answer is not resent: the create may have completed, and a resend
// would then read as a taken slug and mint a second paste. The durable intent
// resolves it instead.
func TestPublishIsNotResentAfterALostAnswer(t *testing.T) {
	f := &fakeCell{reply: func(cellRequest) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }}
	repo := NewPasteRepo("https://cell", f.client())
	if _, err := repo.Publish(context.Background(), createPaste(), 10, time.Now(), ""); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("publish error = %v, want the transport error", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.calls))
	}
}
