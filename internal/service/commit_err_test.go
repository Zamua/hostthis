package service

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestClassifyCommitErr pins the single commit-error translation table
// every write path (upload create, paste update, site deploy/redeploy)
// routes through: the storage triad maps to the service vocabulary,
// bare or wrapped; anything else passes through VERBATIM (the same
// value, so callers can keep wrapping it with their own context).
func TestClassifyCommitErr(t *testing.T) {
	other := errors.New("backend hiccup")
	cases := []struct {
		name      string
		err       error
		wantClass commitErrClass
		wantErr   error // compared with errors.Is; nil means want nil
	}{
		{"nil", nil, commitOK, nil},
		{"service full bare", storage.ErrServiceFull, commitServiceFull, ErrServiceFull},
		{"service full wrapped", fmt.Errorf("blob put: %w", storage.ErrServiceFull), commitServiceFull, ErrServiceFull},
		{"over user quota bare", storage.ErrOverUserQuota, commitOverQuota, ErrOverQuota},
		{"over user quota wrapped", fmt.Errorf("insert: %w", domain.ErrOverUserQuota), commitOverQuota, ErrOverQuota},
		{"slug taken bare", storage.ErrSlugTaken, commitSlugTaken, SlugTakenErr},
		{"slug taken wrapped", fmt.Errorf("preclaim: %w", domain.ErrSlugTaken), commitSlugTaken, SlugTakenErr},
		{"unclassified passes through", other, commitOther, other},
		{"slug lookalike text is unclassified", errors.New("room slug validation failed"), commitOther, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, terr := classifyCommitErr(tc.err)
			if class != tc.wantClass {
				t.Fatalf("classifyCommitErr(%v) class = %v, want %v", tc.err, class, tc.wantClass)
			}
			switch {
			case tc.err == nil:
				if terr != nil {
					t.Fatalf("classifyCommitErr(nil) err = %v, want nil", terr)
				}
			case tc.wantClass == commitOther:
				if terr != tc.err {
					t.Fatalf("classifyCommitErr(%v) err = %v, want the SAME value back", tc.err, terr)
				}
			default:
				if !errors.Is(terr, tc.wantErr) {
					t.Fatalf("classifyCommitErr(%v) err = %v, want %v", tc.err, terr, tc.wantErr)
				}
			}
		})
	}
}
