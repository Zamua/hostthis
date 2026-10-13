package worker_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/workertest"
)

// The room cell answers a client's close frame, so the handshake ends at once
// rather than at the client's own close timeout.
func TestRoomCellAnswersAClientClose(t *testing.T) {
	base := workertest.Target(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	room := url.QueryEscape("close-" + domain.NewRoomID().String())
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/room/join?room="+room, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow() //nolint:errcheck
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	start := time.Now()
	if err := c.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close after %v: %v", time.Since(start), err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("close handshake took %v", took)
	}
}
