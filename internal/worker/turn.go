package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

type turnIssueRequest struct {
	App  string `json:"app"`
	Now  int64  `json:"now"`
	Room string `json:"room"`
}

// IssueICE asks the TURN cell for one participant's ICE servers. Its answer
// is the route's body, passed on byte for byte so both servers serve the
// same document (SPEC.md "TURN relay credentials").
func (r *RoomRepo) IssueICE(app domain.Slug, id domain.RoomID, now time.Time) (json.RawMessage, error) {
	var doc json.RawMessage
	status, err := r.call(context.Background(), opTurnIssue, "", turnIssueRequest{
		App: app.String(), Now: now.UTC().UnixMilli(), Room: id.String(),
	}, &doc)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || len(doc) == 0 {
		return nil, fmt.Errorf("worker: turn issue: unexpected status %d", status)
	}
	return doc, nil
}
