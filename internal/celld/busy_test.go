package celld

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// Every paste mutation refused behind another pending operation is ErrBusy.
func TestMutationsBehindAnotherPendingOperationAreBusy(t *testing.T) {
	busyRepo := func() *PasteRepo {
		return NewPasteRepo("https://cell", (&fakeCell{reply: func(c cellRequest) (*http.Response, error) {
			if c.Path == "/paste/get" {
				return cellResponse(http.StatusOK, `{"slug":"slugone1","generation":"generation-1"}`), nil
			}
			return cellResponse(http.StatusLocked, `{"error":"artifact-operation-pending"}`), nil
		}}).client())
	}
	for name, mutate := range map[string]func() error{
		"append": func() error {
			_, err := busyRepo().AppendVersionWithQuotaCheck(context.Background(), "slugone1", "generation-1",
				domain.KindHTML, "up-v2", domain.Manifest{}, 4, 10, time.Unix(8, 0))
			return err
		},
		"delete version": func() error {
			_, err := busyRepo().DeleteVersion("slugone1", "generation-1", 2)
			return err
		},
		"pin": func() error {
			return busyRepo().SetPinnedVersion("slugone1", "generation-1", domain.Version{VerNum: 1})
		},
		"unpin": func() error { return busyRepo().Unpin("slugone1", "generation-1") },
		"delete": func() error {
			_, err := busyRepo().Delete("slugone1", "key:owner", time.UnixMilli(7))
			return err
		},
		"fail": func() error {
			return busyRepo().MarkFailed(domain.Paste{Slug: "slugone1", Generation: "generation-1"})
		},
	} {
		if err := mutate(); !errors.Is(err, domain.ErrBusy) {
			t.Errorf("%s = %v, want ErrBusy", name, err)
		}
	}
}
