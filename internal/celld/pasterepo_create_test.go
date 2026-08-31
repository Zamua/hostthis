package celld

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func createResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

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
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if request.Body != nil {
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode %s: %v", request.URL.Path, err)
			}
		}
		switch request.URL.Path {
		case "/identity/reserve":
			intent, ok := body["intent"].(map[string]any)
			if !ok {
				t.Fatalf("reserve intent = %#v", body["intent"])
			}
			reserveFingerprint, _ = intent["fingerprint"].(string)
			return createResponse(http.StatusOK, ""), nil
		case "/paste/put":
			putFingerprint, _ = body["fingerprint"].(string)
			return createResponse(http.StatusNoContent, ""), nil
		case "/identity/confirm":
			return createResponse(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	})}

	repo := NewPasteRepo("https://cell", client)
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if reserveFingerprint == "" || reserveFingerprint != putFingerprint {
		t.Fatalf("reserve/put fingerprints = %q/%q, want one non-empty value", reserveFingerprint, putFingerprint)
	}
}

// A lost put response retries the same generation and never releases its reservation.
func TestInsertRetriesAmbiguousPutWithSameGeneration(t *testing.T) {
	var puts []map[string]any
	var releases int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/identity/reserve":
			return createResponse(http.StatusOK, ""), nil
		case "/paste/put":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode put: %v", err)
			}
			puts = append(puts, body)
			if len(puts) == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return createResponse(http.StatusNoContent, ""), nil
		case "/identity/confirm":
			return createResponse(http.StatusNoContent, ""), nil
		case "/identity/release":
			releases++
			return createResponse(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	})}

	repo := NewPasteRepo("https://cell", client)
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if len(puts) != 2 || !reflect.DeepEqual(puts[0], puts[1]) {
		t.Fatalf("put attempts = %#v, want two identical bodies", puts)
	}
	if releases != 0 {
		t.Fatalf("release calls = %d, want 0 after ambiguous put", releases)
	}
}

// Two ambiguous put failures leave the durable intent to resolve the reservation.
func TestInsertDoesNotReleaseAfterAmbiguousPutFailure(t *testing.T) {
	var puts, releases int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/identity/reserve":
			return createResponse(http.StatusOK, ""), nil
		case "/paste/put":
			puts++
			return nil, io.ErrUnexpectedEOF
		case "/identity/release":
			releases++
			return createResponse(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	})}

	repo := NewPasteRepo("https://cell", client)
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
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/identity/reserve":
			return createResponse(http.StatusOK, ""), nil
		case "/paste/put":
			return createResponse(http.StatusConflict, `{"error":"slug-taken"}`), nil
		case "/identity/release":
			releases++
			return createResponse(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	})}

	repo := NewPasteRepo("https://cell", client)
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); !errors.Is(err, domain.ErrSlugTaken) {
		t.Fatalf("insert error = %v, want ErrSlugTaken", err)
	}
	if releases != 1 {
		t.Fatalf("release calls = %d, want 1", releases)
	}
}

// A semantic confirm refusal is surfaced instead of being mistaken for success.
func TestInsertInspectsConfirmStatus(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/identity/reserve":
			return createResponse(http.StatusOK, ""), nil
		case "/paste/put":
			return createResponse(http.StatusNoContent, ""), nil
		case "/identity/confirm":
			return createResponse(http.StatusConflict, `{"error":"generation-mismatch"}`), nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	})}

	repo := NewPasteRepo("https://cell", client)
	if err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now()); err == nil {
		t.Fatal("insert = nil, want semantic confirm error")
	}
}
