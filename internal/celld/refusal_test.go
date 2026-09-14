package celld

// Only an answer the adapter positively identifies may become a definitive
// sentinel. Anything else stays an error the service treats as ambiguous,
// because a definitive refusal deletes the upload's bytes.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func definitive(err error) bool {
	for _, sentinel := range []error{
		domain.ErrNotFound, domain.ErrSlugTaken, domain.ErrOverUserQuota,
		domain.ErrServiceFull, domain.ErrTooManyFiles,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

type cellAnswer struct {
	status int
	body   string
}

func TestCallFailsAnUnlistedStatusWithoutABody(t *testing.T) {
	c := newCell("https://cell", fixedCell(http.StatusInternalServerError, "").client())
	var out struct {
		OK bool `json:"ok"`
	}
	if _, err := c.call(context.Background(), http.MethodGet, "/paste/get", "slug", "slugone1", nil, &out); err == nil {
		t.Fatal("call on an empty-bodied 500 = nil error, want a failure")
	}
}

func appendAnswered(t *testing.T, a cellAnswer) error {
	t.Helper()
	repo := NewPasteRepo("https://cell", fixedCell(a.status, a.body).client())
	_, err := repo.AppendVersionWithQuotaCheck(context.Background(), "slugone1", "generation-1",
		domain.KindHTML, "up-v2", domain.Manifest{}, 4, 10, time.Unix(8, 0))
	return err
}

func TestAppendIsNotFoundOnlyOnAnExplicitAbsentAnswer(t *testing.T) {
	if err := appendAnswered(t, cellAnswer{http.StatusOK, `{"appended":false,"reason":"absent"}`}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("explicit absent answer = %v, want ErrNotFound", err)
	}
	for name, a := range map[string]cellAnswer{
		"unknown op":          {http.StatusNotFound, "unknown op\n"},
		"accounting conflict": {http.StatusConflict, `{"error":"artifact-accounting-conflict"}`},
		"empty object":        {http.StatusOK, `{}`},
		"other reason":        {http.StatusOK, `{"appended":false,"reason":"projection-unavailable"}`},
		"no content":          {http.StatusNoContent, ""},
		"empty server error":  {http.StatusInternalServerError, ""},
	} {
		if err := appendAnswered(t, a); err == nil || definitive(err) {
			t.Errorf("%s: append = %v, want an ambiguous error", name, err)
		}
	}
}

func TestInsertMapsOnlyIdentifiedConflictsToSlugTaken(t *testing.T) {
	for _, tc := range []struct {
		name, step, body string
		taken            bool
	}{
		{"reserve slug taken", "/identity/reserve", `{"error":"slug-taken"}`, true},
		{"reserve mismatch", "/identity/reserve", `{"error":"reservation-mismatch"}`, false},
		{"put slug taken", "/paste/put", `{"error":"slug-taken"}`, true},
		{"put aborted", "/paste/put", `{"error":"create-aborted"}`, true},
		{"put mismatch", "/paste/put", `{"error":"create-mismatch"}`, false},
		{"put bare conflict", "/paste/put", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var releases int
			f := &fakeCell{reply: func(c cellRequest) (*http.Response, error) {
				if c.Path == tc.step {
					return cellResponse(http.StatusConflict, tc.body), nil
				}
				switch c.Path {
				case "/identity/reserve":
					return cellResponse(http.StatusOK, ""), nil
				case "/paste/put", "/identity/confirm":
					return cellResponse(http.StatusNoContent, ""), nil
				case "/identity/release":
					releases++
					return cellResponse(http.StatusNoContent, ""), nil
				}
				t.Fatalf("unexpected path %q", c.Path)
				return nil, nil
			}}
			repo := NewPasteRepo("https://cell", f.client())
			err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now())
			if err == nil || errors.Is(err, domain.ErrSlugTaken) != tc.taken || (!tc.taken && definitive(err)) {
				t.Fatalf("insert = %v, want slug taken %v", err, tc.taken)
			}
			wantReleases := 0
			if tc.taken && tc.step == "/paste/put" {
				wantReleases = 1
			}
			if releases != wantReleases {
				t.Fatalf("releases = %d, want %d", releases, wantReleases)
			}
		})
	}
}

func TestDeleteVersionReadsTheServedRefusalFromItsBody(t *testing.T) {
	repo := NewPasteRepo("https://cell", fixedCell(http.StatusConflict, `{"error":"version-served"}`).client())
	if _, err := repo.DeleteVersion("slugone1", "generation-1", 2); !errors.Is(err, domain.ErrVersionCurrentlyServed) {
		t.Fatalf("served refusal = %v, want ErrVersionCurrentlyServed", err)
	}
	repo = NewPasteRepo("https://cell", fixedCell(http.StatusConflict, `{"error":"artifact-accounting-conflict"}`).client())
	if _, err := repo.DeleteVersion("slugone1", "generation-1", 2); err == nil ||
		errors.Is(err, domain.ErrVersionCurrentlyServed) || definitive(err) {
		t.Fatalf("accounting conflict = %v, want an unclassified error", err)
	}
}
