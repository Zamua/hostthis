package worker

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// Each request body encodes to the JSON object the Worker reads: same keys,
// same values, same JSON types, and absent keys stay absent.
func TestRequestBodiesKeepTheWireShape(t *testing.T) {
	base := 3
	row := pasteRow{Slug: "slugone1", Identity: "id1", Status: "pending", Kind: "html"}
	items := []domain.PushItem{}
	for _, tc := range []struct {
		name string
		body any
		want map[string]any
	}{
		{"reserve", reserveRequest{
			Generation: "g1", Kind: "html", Name: "n", Now: 5, Size: 7, Slug: "s1",
			Status: domain.PasteStatusPending, UpdatedAt: 6, UserCap: 100,
			Intent: createIntent{Fingerprint: "f", ID: "i", Kind: intentCreatePaste, StartedAt: 5, Subject: "s1"},
		}, map[string]any{
			"slug": "s1", "generation": "g1", "size": 7, "userCap": 100, "now": 5, "status": "pending",
			"updatedAt": 6, "kind": "html", "name": "n",
			"intent": map[string]any{"id": "i", "kind": "create_paste", "subject": "s1", "fingerprint": "f", "startedAt": 5},
		}},
		{"release", releaseRequest{Generation: "g1", IntentID: "i", Slug: "s1"},
			map[string]any{"slug": "s1", "generation": "g1", "intentId": "i"}},
		{"confirm", confirmRequest{Generation: "g1", IntentID: "i", Slug: "s1", Status: domain.PasteStatusReady},
			map[string]any{"slug": "s1", "generation": "g1", "status": "ready", "intentId": "i"}},
		{"drop", dropRequest{Slug: "s1"}, map[string]any{"slug": "s1"}},
		{"note subnet", noteSubnetRequest{Now: 5, Subnet: "10.0.0.0/24"},
			map[string]any{"subnet": "10.0.0.0/24", "now": 5}},
		{"link start", linkStartRequest{Expires: 9, Owner: "o", SecretHash: "h", WebID: "w"},
			map[string]any{"owner": "o", "webid": "w", "secretHash": "h", "expires": 9}},
		{"pod clear", podClearRequest{}, map[string]any{}},
		{"paste put", pastePutRequest{Fingerprint: "f", Generation: "g1", Row: row},
			map[string]any{"row": row, "generation": "g1", "fingerprint": "f"}},
		{"ready", pasteStatusRequest{Generation: "g1", Status: domain.PasteStatusReady},
			map[string]any{"status": "ready", "generation": "g1"}},
		{"failed", pasteStatusRequest{Generation: "g1", OpID: "fail:g1", Status: domain.PasteStatusFailed},
			map[string]any{"status": "failed", "generation": "g1", "opId": "fail:g1"}},
		{"rename", renameRequest{CreatedAt: 4, Identity: "id1", Name: "n"},
			map[string]any{"name": "n", "identity": "id1", "createdAt": 4}},
		{"remove", removeRequest{CreatedAt: 4, Generation: "g1", Identity: "id1", OpID: "op"},
			map[string]any{"opId": "op", "generation": "g1", "identity": "id1", "createdAt": 4}},
		{"append", appendRequest{
			Generation: "g1", Kind: "html", Manifest: domain.Manifest{}, Now: 5, OpID: "op",
			Size: 7, UploadID: "u", UserCap: 100,
		}, map[string]any{
			"opId": "op", "generation": "g1", "userCap": 100, "kind": "html", "uploadId": "u",
			"size": 7, "manifest": domain.Manifest{}, "now": 5,
		}},
		{"append on a base", appendRequest{Base: &base, Generation: "g1", Manifest: domain.Manifest{}},
			map[string]any{
				"opId": "", "generation": "g1", "userCap": 0, "kind": "", "uploadId": "",
				"size": 0, "manifest": domain.Manifest{}, "now": 0, "base": 3,
			}},
		{"version", versionRequest{Generation: "g1", OpID: "op", Ver: 0},
			map[string]any{"opId": "op", "generation": "g1", "ver": 0}},
		{"admit", admitRequest{Identity: "id1", Limit: 3, Now: 5, Window: 60},
			map[string]any{"identity": "id1", "now": 5, "window": 60, "limit": 3}},
		{"room create", roomCreateRequest{AppCap: 9, AppSlug: "a", At: 5, CreatedAt: 4, ID: "r", RoomLimit: 7, Subnet: "n", UpdatedAt: 6},
			map[string]any{"appSlug": "a", "id": "r", "createdAt": 4, "updatedAt": 6, "subnet": "n", "at": 5, "appCap": 9, "roomLimit": 7}},
		{"room put", roomPutRequest{AppCap: 9, Key: "k", KeyCap: 2, Now: 5, RoomCap: 8, Value: "dg==", Wire: `"v"`},
			map[string]any{"key": "k", "value": "dg==", "wire": `"v"`, "roomCap": 8, "keyCap": 2, "appCap": 9, "now": 5}},
		{"room delete", roomDelRequest{Key: "k", Now: 5}, map[string]any{"key": "k", "now": 5}},
		{"push subscription put", pushSubPutRequest{
			Endpoint: "e", Keys: pushKeys{Auth: "a", P256DH: "p"}, Now: 5, Subject: "s",
		}, map[string]any{
			"endpoint": "e", "keys": map[string]string{"p256dh": "p", "auth": "a"}, "now": 5, "subject": "s",
		}},
		{"push subscription delete", pushSubDelRequest{Endpoint: "e"}, map[string]any{"endpoint": "e"}},
		{"push schedule put", pushScheduleRequest{Items: items, Now: 5, Subject: "s", TZ: "UTC"},
			map[string]any{"tz": "UTC", "items": items, "now": 5, "subject": "s"}},
		{"push test", pushTestRequest{Now: 5}, map[string]any{"now": 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := decoded(t, tc.body), decoded(t, tc.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("body = %v, want %v", got, want)
			}
		})
	}
}

func decoded(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
