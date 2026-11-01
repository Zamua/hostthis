package worker

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// The Go side of the Worker's cell protocol (worker/src): every op's route
// and request body. Body fields are declared in JSON key order, so a body
// encodes to the same bytes a sorted map would.

// routingKey is the query param naming the cell an op addresses.
type routingKey string

const (
	byScope  routingKey = "scope"
	bySlug   routingKey = "slug"
	byRoom   routingKey = "room"
	bySubnet routingKey = "subnet"
	// singleton addresses the one cell of a namespace, so no param names it.
	singleton routingKey = ""
)

type route struct {
	method string
	path   string
	key    routingKey
}

// withQuery adds params, already escaped, ahead of the routing param.
func (rt route) withQuery(q string) route {
	rt.path += "?" + q
	return rt
}

// target is the path and query addressing cell val.
func (rt route) target(val string) string {
	// Merged rather than appended: a second bare "?" would make the whole
	// string one unparseable query.
	if rt.key == singleton {
		return rt.path
	}
	sep := "?"
	if strings.Contains(rt.path, "?") {
		sep = "&"
	}
	return rt.path + sep + string(rt.key) + "=" + url.QueryEscape(val)
}

var (
	opIdentityReserve    = route{http.MethodPost, "/identity/reserve", byScope}
	opIdentityRelease    = route{http.MethodPost, "/identity/release", byScope}
	opIdentityConfirm    = route{http.MethodPost, "/identity/confirm", byScope}
	opIdentityBytes      = route{http.MethodGet, "/identity/bytes", byScope}
	opIdentityList       = route{http.MethodGet, "/identity/list", byScope}
	opIdentityFirstSeen  = route{http.MethodGet, "/identity/firstSeen", byScope}
	opIdentityDrop       = route{http.MethodPost, "/identity/drop", byScope}
	opIdentityNoteSubnet = route{http.MethodPost, "/identity/notesubnet", byScope}
	opIdentitySubnets    = route{http.MethodGet, "/identity/subnets", byScope}
	opIdentityPodGet     = route{http.MethodGet, "/identity/podget", byScope}
	opIdentityPodClear   = route{http.MethodPost, "/identity/podclear", byScope}
	opIdentityLinkStart  = route{http.MethodPost, "/identity/linkstart", byScope}

	opPastePut        = route{http.MethodPost, "/paste/put", bySlug}
	opPasteGet        = route{http.MethodGet, "/paste/get", bySlug}
	opPasteStatus     = route{http.MethodPost, "/paste/status", bySlug}
	opPasteRename     = route{http.MethodPost, "/paste/rename", bySlug}
	opPasteRemove     = route{http.MethodPost, "/paste/remove", bySlug}
	opPasteAppend     = route{http.MethodPost, "/paste/append", bySlug}
	opPasteVersions   = route{http.MethodGet, "/paste/versions", bySlug}
	opPasteDelVersion = route{http.MethodPost, "/paste/delversion", bySlug}
	opPasteHead       = route{http.MethodGet, "/paste/head", bySlug}
	opPastePin        = route{http.MethodPost, "/paste/pin", bySlug}
	opPastePushKey    = route{http.MethodPost, "/paste/pushkey", bySlug}
	opPasteRoomCounts = route{http.MethodGet, "/paste/roomcounts", bySlug}

	opSubnetAdmit    = route{http.MethodPost, "/subnet/admit", bySubnet}
	opSubnetSnapshot = route{http.MethodGet, "/subnet/snapshot", bySubnet}

	opDenylistList = route{http.MethodGet, "/denylist/list", singleton}

	opRoomCreate          = route{http.MethodPost, "/room/create", byRoom}
	opRoomMeta            = route{http.MethodGet, "/room/meta", byRoom}
	opRoomGet             = route{http.MethodGet, "/room/get", byRoom}
	opRoomScan            = route{http.MethodGet, "/room/scan", byRoom}
	opRoomPut             = route{http.MethodPost, "/room/put", byRoom}
	opRoomDel             = route{http.MethodPost, "/room/del", byRoom}
	opRoomJoin            = route{http.MethodGet, "/room/join", byRoom}
	opRoomPushSubPut      = route{http.MethodPost, "/room/pushsubput", byRoom}
	opRoomPushSubDel      = route{http.MethodPost, "/room/pushsubdel", byRoom}
	opRoomPushSubList     = route{http.MethodPost, "/room/pushsublist", byRoom}
	opRoomPushSchedulePut = route{http.MethodPost, "/room/pushscheduleput", byRoom}
	opRoomPushScheduleGet = route{http.MethodPost, "/room/pushscheduleget", byRoom}
	opRoomPushTest        = route{http.MethodPost, "/room/pushtest", byRoom}
)

// Refusal codes a cell answers in its `error` (or `reason`) field.
const (
	codeSlugTaken            = "slug-taken"
	codeCreateAborted        = "create-aborted"
	codeGenerationMismatch   = "generation-mismatch"
	codeBaseMoved            = "base-moved"
	codeBadReuse             = "bad-reuse"
	codeVersionServed        = "version-served"
	codePodsUnavailable      = "pods-unavailable"
	codeTooManySubscriptions = "too-many-subscriptions"
	codeTooManyItems         = "too-many-items"
	reasonAbsent             = "absent"
)

// refusal is the body of a cell's refusal answer.
type refusal struct {
	Error string `json:"error"`
}

// --- identity cell ----------------------------------------------------------

const intentCreatePaste = "create_paste"

type reserveRequest struct {
	Generation string             `json:"generation"`
	Intent     createIntent       `json:"intent"`
	Kind       domain.ContentKind `json:"kind"`
	Name       string             `json:"name"`
	Now        int64              `json:"now"`
	Size       int                `json:"size"`
	Slug       string             `json:"slug"`
	Status     domain.PasteStatus `json:"status"`
	UpdatedAt  int64              `json:"updatedAt"`
	UserCap    int64              `json:"userCap"`
}

type createIntent struct {
	Fingerprint string `json:"fingerprint"`
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	StartedAt   int64  `json:"startedAt"`
	Subject     string `json:"subject"`
}

type releaseRequest struct {
	Generation string `json:"generation"`
	IntentID   string `json:"intentId"`
	Slug       string `json:"slug"`
}

type confirmRequest struct {
	Generation string             `json:"generation"`
	IntentID   string             `json:"intentId"`
	Slug       string             `json:"slug"`
	Status     domain.PasteStatus `json:"status"`
}

type dropRequest struct {
	Slug string `json:"slug"`
}

type noteSubnetRequest struct {
	Now    int64  `json:"now"`
	Subnet string `json:"subnet"`
}

type linkStartRequest struct {
	Expires    int64  `json:"expires"`
	Owner      string `json:"owner"`
	SecretHash string `json:"secretHash"`
	WebID      string `json:"webid"`
}

type podClearRequest struct{}

// --- paste cell -------------------------------------------------------------

type pastePutRequest struct {
	Fingerprint string   `json:"fingerprint"`
	Generation  string   `json:"generation"`
	Row         pasteRow `json:"row"`
}

type pasteStatusRequest struct {
	Generation string `json:"generation"`
	// Set only by an op the cell must replay idempotently.
	OpID   string             `json:"opId,omitempty"`
	Status domain.PasteStatus `json:"status"`
}

type renameRequest struct {
	CreatedAt int64  `json:"createdAt"`
	Identity  string `json:"identity"`
	Name      string `json:"name"`
}

type removeRequest struct {
	CreatedAt  int64  `json:"createdAt"`
	Generation string `json:"generation"`
	Identity   string `json:"identity"`
	OpID       string `json:"opId"`
}

type appendRequest struct {
	// Absent unless the version was computed against one.
	Base       *int               `json:"base,omitempty"`
	Generation string             `json:"generation"`
	Kind       domain.ContentKind `json:"kind"`
	Manifest   domain.Manifest    `json:"manifest"`
	Now        int64              `json:"now"`
	OpID       string             `json:"opId"`
	Size       int                `json:"size"`
	UploadID   string             `json:"uploadId"`
	UserCap    int64              `json:"userCap"`
}

// versionRequest addresses one version: a delete or a pin (0 unpins).
type versionRequest struct {
	Generation string `json:"generation"`
	OpID       string `json:"opId"`
	Ver        int    `json:"ver"`
}

// --- subnet cell ------------------------------------------------------------

type admitRequest struct {
	Identity string `json:"identity"`
	Limit    int    `json:"limit"`
	Now      int64  `json:"now"`
	Window   int64  `json:"window"`
}

// --- room cell --------------------------------------------------------------

type roomCreateRequest struct {
	AppCap    int64  `json:"appCap"`
	AppSlug   string `json:"appSlug"`
	At        int64  `json:"at"`
	CreatedAt int64  `json:"createdAt"`
	ID        string `json:"id"`
	RoomLimit int    `json:"roomLimit"`
	Subnet    string `json:"subnet"`
	UpdatedAt int64  `json:"updatedAt"`
}

type roomPutRequest struct {
	AppCap  int64  `json:"appCap"`
	Key     string `json:"key"`
	KeyCap  int    `json:"keyCap"`
	Now     int64  `json:"now"`
	RoomCap int    `json:"roomCap"`
	// Base64 of the stored bytes.
	Value string `json:"value"`
	// The frame encoding the cell echoes verbatim (domain.RoomWireValue).
	Wire string `json:"wire"`
}

type roomDelRequest struct {
	Key string `json:"key"`
	Now int64  `json:"now"`
}

type pushSubPutRequest struct {
	Endpoint string   `json:"endpoint"`
	Keys     pushKeys `json:"keys"`
	Now      int64    `json:"now"`
	Subject  string   `json:"subject"`
}

type pushKeys struct {
	Auth   string `json:"auth"`
	P256DH string `json:"p256dh"`
}

type pushSubDelRequest struct {
	Endpoint string `json:"endpoint"`
}

type pushScheduleRequest struct {
	Items   []domain.PushItem `json:"items"`
	Now     int64             `json:"now"`
	Subject string            `json:"subject"`
	TZ      string            `json:"tz"`
}

type pushTestRequest struct {
	Now int64 `json:"now"`
}
