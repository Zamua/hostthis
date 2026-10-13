package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/roomwire"
	"github.com/Zamua/hostthis/internal/workertest"
)

// probeWorker builds the Worker clients exactly as the daemon does, from
// HOSTTHIS_WORKER_ENDPOINT and HOSTTHIS_WORKER_TOKEN, and drives each of them
// once: every metadata adapter, the three blob calls, and the room relay dial.
// A probe's error is nil when the Worker served it.
func probeWorker(t *testing.T, logger *log.Logger) map[string]error {
	t.Helper()
	meta, err := buildMetadata("example.test", logger)
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	blobs, err := buildBlobStore(logger)
	if err != nil {
		t.Fatalf("build blob store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	slug := domain.Slug("prbe2345")
	notFound := func(err error) error {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err == nil {
			return errors.New("found a record the test never wrote")
		}
		return err
	}
	probes := map[string]error{}

	_, err = meta.Repo.Get(slug)
	probes["paste repo"] = notFound(err)
	_, err = meta.Sites.Get(slug)
	probes["sites"] = notFound(err)
	_, err = meta.KeyGate.AdmitNewKey("SHA256:token-probe", "203.0.113.0/24", time.Now(), 20, time.Hour)
	probes["key gate"] = err
	_, err = meta.Rooms.GetRoom(slug, domain.NewRoomID())
	probes["room repo"] = notFound(err)

	upload := domain.NewUploadID()
	key := domain.UploadObjectKey(upload, 0)
	_, err = blobs.StageEncoding(ctx, key, strings.NewReader("payload"))
	probes["blob put"] = err
	rc, _, err := blobs.Read(ctx, domain.ManifestEntry{Key: key})
	if err == nil {
		var got []byte
		got, err = io.ReadAll(rc)
		rc.Close() //nolint:errcheck
		if err == nil && string(got) != "payload" {
			err = errors.New("blob read back " + string(got))
		}
	}
	probes["blob get"] = err
	probes["blob delete"] = blobs.DeleteUpload(ctx, upload)

	probes["room relay"] = probeRelay(ctx, t, meta)
	return probes
}

// probeRelay joins a room through the relay and reads the cell's snapshot.
func probeRelay(ctx context.Context, t *testing.T, meta *metadataBundle) error {
	t.Helper()
	room := roomwire.RoomKey{App: "prbe2345", ID: domain.NewRoomID()}
	id, err := meta.RoomRelay.Admit(room)
	if err != nil {
		return err
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err == nil {
			meta.RoomRelay.Serve(r.Context(), room, id, c)
		}
	}))
	defer front.Close()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		return err
	}
	defer client.CloseNow() //nolint:errcheck
	_, _, err = client.Read(ctx)
	return err
}

func TestEveryWorkerCallCarriesTheToken(t *testing.T) {
	endpoint, token := workertest.TokenEndpoint(t)
	t.Setenv("HOSTTHIS_WORKER_ENDPOINT", endpoint)
	t.Setenv("HOSTTHIS_WORKER_TOKEN", token)
	for name, err := range probeWorker(t, log.New(io.Discard, "", 0)) {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Each probe reaches the token check: without the token every one is refused,
// so the test above cannot pass by touching nothing.
func TestWorkerCallsWithoutTheTokenAreRefused(t *testing.T) {
	endpoint, _ := workertest.TokenEndpoint(t)
	t.Setenv("HOSTTHIS_WORKER_ENDPOINT", endpoint)
	t.Setenv("HOSTTHIS_WORKER_TOKEN", "")
	for name, err := range probeWorker(t, log.New(io.Discard, "", 0)) {
		if err == nil {
			t.Errorf("%s: served without the token", name)
		}
	}
}

func TestTheTokenStaysOutOfErrorsAndLogs(t *testing.T) {
	endpoint, token := workertest.TokenEndpoint(t)
	wrong := "wrong-" + token
	t.Setenv("HOSTTHIS_WORKER_ENDPOINT", endpoint)
	t.Setenv("HOSTTHIS_WORKER_TOKEN", wrong)
	var logs bytes.Buffer
	for name, err := range probeWorker(t, log.New(&logs, "", 0)) {
		if err == nil {
			t.Errorf("%s: served with a wrong token", name)
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("%s: error %q carries the token", name, err)
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("logs carry the token:\n%s", logs.String())
	}
}
