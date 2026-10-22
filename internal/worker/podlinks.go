package worker

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// Pod links live on the owner's identity cell (the active link) and on one
// cell per link, `podlink:<id>`, which the Worker's payload store and link
// flow read (docs/SPEC.md "Pod storage").

type podLinkRow struct {
	ID      string `json:"id"`
	WebID   string `json:"webid"`
	Storage string `json:"storage"`
	Linked  int64  `json:"linked"`
}

func podLinkCell(id string) string { return "podlink:" + id }

// ActivePodLink is the owner's linked pod, or nil.
func (r *PasteRepo) ActivePodLink(owner string) (*domain.PodLink, error) {
	var res struct {
		Link *podLinkRow `json:"link"`
	}
	if err := r.ask(context.Background(), "identity podget", http.MethodGet, "/identity/podget", "scope", owner,
		nil, &res, nil); err != nil {
		return nil, err
	}
	if res.Link == nil {
		return nil, nil
	}
	return &domain.PodLink{
		ID:      res.Link.ID,
		WebID:   res.Link.WebID,
		Storage: res.Link.Storage,
		Linked:  time.UnixMilli(res.Link.Linked).UTC(),
	}, nil
}

// StartPodLink records a pending link the owner completes in a browser.
func (r *PasteRepo) StartPodLink(owner, webid, linkID, secretHash string, expires time.Time) error {
	var refusal struct {
		Error string `json:"error"`
	}
	status, err := r.call(context.Background(), http.MethodPost, "/identity/linkstart", "scope", podLinkCell(linkID),
		map[string]any{
			"owner":      owner,
			"webid":      webid,
			"secretHash": secretHash,
			"expires":    expires.UnixMilli(),
		}, &refusal)
	if err != nil {
		return err
	}
	if status == http.StatusConflict && refusal.Error == "pods-unavailable" {
		return domain.ErrPodsUnavailable
	}
	if status >= 300 {
		return fmt.Errorf("worker: link start: status %d %s", status, refusal.Error)
	}
	return nil
}

// ClearPodLink unlinks the owner's pod and reports whether there was one.
func (r *PasteRepo) ClearPodLink(owner string) (bool, error) {
	var res struct {
		Cleared bool `json:"cleared"`
	}
	if err := r.ask(context.Background(), "identity podclear", http.MethodPost, "/identity/podclear", "scope", owner,
		map[string]any{}, &res, nil); err != nil {
		return false, err
	}
	return res.Cleared, nil
}
