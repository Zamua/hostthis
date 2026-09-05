package celld

import (
	"bytes"
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
		ContentSHA: "sha", Size: 3, CreatedAt: at, UpdatedAt: at,
	}
}

func TestInsertCarriesOneCreationFingerprint(t *testing.T) {
	var reserveFingerprint, putFingerprint string
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/identity/reserve":
			intent, ok := c.fields(t)["intent"].(map[string]any)
			if !ok {
				t.Fatalf("reserve intent = %#v", c.fields(t)["intent"])
			}
			reserveFingerprint, _ = intent["fingerprint"].(string)
			return cellResponse(http.StatusOK, ""), nil
		case "/paste/put":
			putFingerprint, _ = c.fields(t)["fingerprint"].(string)
			return cellResponse(http.StatusNoContent, ""), nil
		case "/identity/confirm":
			return cellResponse(http.StatusNoContent, ""), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}

	repo := NewPasteRepo("https://cell", f.client())
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if reserveFingerprint == "" || reserveFingerprint != putFingerprint {
		t.Fatalf("reserve/put fingerprints = %q/%q, want one non-empty value", reserveFingerprint, putFingerprint)
	}
}

// A lost put response retries the same generation and never releases its reservation.
func TestInsertRetriesAmbiguousPutWithSameGeneration(t *testing.T) {
	var puts []cellRequest
	var releases int
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/identity/reserve":
			return cellResponse(http.StatusOK, ""), nil
		case "/paste/put":
			puts = append(puts, c)
			if len(puts) == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return cellResponse(http.StatusNoContent, ""), nil
		case "/identity/confirm":
			return cellResponse(http.StatusNoContent, ""), nil
		case "/identity/release":
			releases++
			return cellResponse(http.StatusNoContent, ""), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}

	repo := NewPasteRepo("https://cell", f.client())
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if len(puts) != 2 || !bytes.Equal(puts[0].Body, puts[1].Body) {
		t.Fatalf("put attempts = %#v, want two identical bodies", puts)
	}
	if releases != 0 {
		t.Fatalf("release calls = %d, want 0 after ambiguous put", releases)
	}
}

// Two ambiguous put failures leave the durable intent to resolve the reservation.
func TestInsertDoesNotReleaseAfterAmbiguousPutFailure(t *testing.T) {
	var puts, releases int
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/identity/reserve":
			return cellResponse(http.StatusOK, ""), nil
		case "/paste/put":
			puts++
			return nil, io.ErrUnexpectedEOF
		case "/identity/release":
			releases++
			return cellResponse(http.StatusNoContent, ""), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}

	repo := NewPasteRepo("https://cell", f.client())
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("insert error = %v, want transport error", err)
	}
	if puts != 2 || releases != 0 {
		t.Fatalf("put/release calls = %d/%d, want 2/0", puts, releases)
	}
}

// A definitive generation conflict releases the reservation.
func TestInsertReleasesAfterDefinitivePutConflict(t *testing.T) {
	var releases int
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/identity/reserve":
			return cellResponse(http.StatusOK, ""), nil
		case "/paste/put":
			return cellResponse(http.StatusConflict, `{"error":"slug-taken"}`), nil
		case "/identity/release":
			releases++
			return cellResponse(http.StatusNoContent, ""), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}

	repo := NewPasteRepo("https://cell", f.client())
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); !errors.Is(err, domain.ErrSlugTaken) {
		t.Fatalf("insert error = %v, want ErrSlugTaken", err)
	}
	if releases != 1 {
		t.Fatalf("release calls = %d, want 1", releases)
	}
}

// A semantic confirm refusal is surfaced instead of being mistaken for success.
func TestInsertInspectsConfirmStatus(t *testing.T) {
	f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
		switch c.Path {
		case "/identity/reserve":
			return cellResponse(http.StatusOK, ""), nil
		case "/paste/put":
			return cellResponse(http.StatusNoContent, ""), nil
		case "/identity/confirm":
			return cellResponse(http.StatusConflict, `{"error":"generation-mismatch"}`), nil
		}
		t.Fatalf("unexpected path %q", c.Path)
		return nil, nil
	}}

	repo := NewPasteRepo("https://cell", f.client())
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); err == nil {
		t.Fatal("insert = nil, want semantic confirm error")
	}
}
