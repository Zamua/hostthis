package celld

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// MarkReady reports a paste that is gone, absent or re-minted, as not found.
func TestMarkReadyReportsAPasteThatIsGone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		answer     cellAnswer
		gone, fail bool
	}{
		{"absent", cellAnswer{http.StatusOK, `{"changed":false,"reason":"absent"}`}, true, false},
		{"re-minted", cellAnswer{http.StatusConflict, `{"error":"generation-mismatch"}`}, true, false},
		{"flipped", cellAnswer{http.StatusOK, `{"changed":true,"status":"ready"}`}, false, false},
		{"already settled", cellAnswer{http.StatusOK, `{"changed":false,"reason":"not-pending","status":"failed"}`}, false, false},
		{"unidentified conflict", cellAnswer{http.StatusConflict, `{"error":"other"}`}, false, true},
	} {
		repo := NewPasteRepo("https://cell", fixedCell(tc.answer.status, tc.answer.body).client())
		err := repo.MarkReady(domain.Paste{Slug: "slugone1", Generation: "generation-1"})
		if errors.Is(err, domain.ErrNotFound) != tc.gone || (err != nil) != (tc.gone || tc.fail) {
			t.Errorf("%s: MarkReady = %v, want gone %v, failure %v", tc.name, err, tc.gone, tc.fail)
		}
	}
}
