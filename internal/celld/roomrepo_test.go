package celld

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celldtest"
	"github.com/Zamua/hostthis/internal/domain"
)

func TestRoomRepoCreateMapsFullApp(t *testing.T) {
	f := fixedCell(http.StatusInsufficientStorage, `{"error":"app-full"}`)
	now := time.Unix(1, 0).UTC()
	err := NewRoomRepo("https://cell", f.client()).CreateRoom(domain.Room{
		AppSlug:   domain.Slug("abcdefgh"),
		ID:        domain.RoomID("123e4567-e89b-42d3-a456-426614174000"),
		CreatedAt: now,
		UpdatedAt: now,
	}, "192.0.2.0/24", 123, now)
	if !errors.Is(err, domain.ErrAppRoomsFull) {
		t.Fatalf("CreateRoom error = %v", err)
	}
	if req := f.last(); req.Path != "/room/create" || req.fields(t)["appCap"] != float64(123) {
		t.Fatalf("request = %s appCap %#v, want /room/create with 123", req.Path, req.fields(t)["appCap"])
	}
}

// The scan carries the cell's text per value; text that is not JSON gives way
// to the bytes' own encoding.
func TestRoomRepoScanCarriesTheStoredText(t *testing.T) {
	f := fixedCell(http.StatusOK, `{"values":{"a":"MWU0MDA=","b":"MQ==","c":"PA=="},"seq":3,`+
		`"wire":{"a":"1e400","b":"not json","c":"\"\\u003c\""}}`)
	kv, err := NewRoomRepo("https://cell", f.client()).ScanRoom("abcdefgh", "123e4567-e89b-42d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"a": "1e400", "b": "1", "c": `"\u003c"`} {
		if got := string(kv.WireValue(key)); got != want {
			t.Errorf("%s: %s, want %s", key, got, want)
		}
	}
}

// The cell's {created:false} is an id already taken, the error the service
// retries with a fresh id.
func TestRoomRepoCreateReportsATakenID(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	room := domain.Room{
		AppSlug: domain.Slug("abcdefgh"), ID: domain.RoomID("123e4567-e89b-42d3-a456-426614174000"),
		CreatedAt: now, UpdatedAt: now,
	}
	for body, want := range map[string]error{`{"created":true}`: nil, `{"created":false}`: domain.ErrSlugTaken} {
		err := NewRoomRepo("https://cell", fixedCell(http.StatusOK, body).client()).CreateRoom(room, "192.0.2.0/24", 0, now)
		if !errors.Is(err, want) || (want == nil && err != nil) {
			t.Fatalf("%s: CreateRoom error = %v, want %v", body, err, want)
		}
	}

	// Against the Worker: the second create of one id is the taken one.
	repo := NewRoomRepo(celldtest.Endpoint(t), nil)
	room.ID = domain.NewRoomID()
	if err := repo.CreateRoom(room, "192.0.2.0/24", 0, now); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := repo.CreateRoom(room, "192.0.2.0/24", 0, now); !errors.Is(err, domain.ErrSlugTaken) {
		t.Fatalf("second create = %v, want ErrSlugTaken", err)
	}
}
