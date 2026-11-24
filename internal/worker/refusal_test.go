package worker

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
		domain.ErrTooManyFiles, domain.ErrBusy,
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
	if _, err := c.call(context.Background(), opPasteGet, "slugone1", nil, &out); err == nil {
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
		name, body string
		taken      bool
	}{
		{"slug taken", `{"error":"slug-taken"}`, true},
		{"aborted", `{"error":"create-aborted"}`, true},
		{"reservation mismatch", `{"error":"reservation-mismatch"}`, false},
		{"create mismatch", `{"error":"create-mismatch"}`, false},
		{"bare conflict", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := NewPasteRepo("https://cell", fixedCell(http.StatusConflict, tc.body).client())
			err := repo.InsertWithQuotaCheck(context.Background(), createPaste(), 10, time.Now())
			if err == nil || errors.Is(err, domain.ErrSlugTaken) != tc.taken || (!tc.taken && definitive(err)) {
				t.Fatalf("insert = %v, want slug taken %v", err, tc.taken)
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

// The cell's refusal of a call addressed to a replaced incarnation is
// definitive: nothing was written, and the paste the caller named is gone.
func TestMutationsOnAReplacedIncarnationAreNotFound(t *testing.T) {
	replacedRepo := func() *PasteRepo {
		return NewPasteRepo("https://cell", (&fakeCell{reply: func(c cellRequest) (*http.Response, error) {
			if c.Path == "/paste/get" {
				return cellResponse(http.StatusOK, `{"slug":"slugone1","generation":"generation-2"}`), nil
			}
			return cellResponse(http.StatusConflict, `{"error":"generation-mismatch"}`), nil
		}}).client())
	}
	for name, mutate := range map[string]func() error{
		"append": func() error {
			_, err := replacedRepo().AppendVersionWithQuotaCheck(context.Background(), "slugone1", "generation-1",
				domain.KindHTML, "up-v2", domain.Manifest{}, 4, 10, time.Unix(8, 0))
			return err
		},
		"delete version": func() error {
			_, err := replacedRepo().DeleteVersion("slugone1", "generation-1", 2)
			return err
		},
		"pin": func() error {
			return replacedRepo().SetPinnedVersion("slugone1", "generation-1", domain.Version{VerNum: 1})
		},
		"unpin": func() error { return replacedRepo().Unpin("slugone1", "generation-1") },
		"delete": func() error {
			_, err := replacedRepo().Delete("slugone1", "key:owner", time.UnixMilli(7))
			return err
		},
	} {
		if err := mutate(); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
}
