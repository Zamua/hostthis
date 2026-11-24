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
	noPod := ""
	row := pasteRow{Slug: "slugone1", Identity: "id1", Status: "pending", Kind: "html"}
	for _, tc := range []struct {
		name string
		body any
		want map[string]any
	}{
		{"publish", publishRequest{
			Fingerprint: "f", Generation: "g1", IntentID: "i", Now: 5, Row: row, UserCap: 100,
		}, map[string]any{
			"row": row, "generation": "g1", "fingerprint": "f", "intentId": "i", "userCap": 100, "now": 5,
		}},
		{"publish naming no pod", publishRequest{Generation: "g1", Pod: &noPod, Row: row},
			map[string]any{
				"row": row, "generation": "g1", "fingerprint": "", "intentId": "", "userCap": 0, "now": 0, "pod": "",
			}},
		{"drop", dropRequest{Slug: "s1"}, map[string]any{"slug": "s1"}},
		{"note subnet", noteSubnetRequest{Now: 5, Subnet: "10.0.0.0/24"},
			map[string]any{"subnet": "10.0.0.0/24", "now": 5}},
		{"link start", linkStartRequest{Expires: 9, Owner: "o", SecretHash: "h", WebID: "w"},
			map[string]any{"owner": "o", "webid": "w", "secretHash": "h", "expires": 9}},
		{"pod clear", podClearRequest{}, map[string]any{}},
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
