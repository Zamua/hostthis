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
	opIdentityBytes      = route{http.MethodGet, "/identity/bytes", byScope}
	opIdentityList       = route{http.MethodGet, "/identity/list", byScope}
	opIdentityFirstSeen  = route{http.MethodGet, "/identity/firstSeen", byScope}
	opIdentityDrop       = route{http.MethodPost, "/identity/drop", byScope}
	opIdentityNoteSubnet = route{http.MethodPost, "/identity/notesubnet", byScope}
	opIdentitySubnets    = route{http.MethodGet, "/identity/subnets", byScope}
	opIdentityPodGet     = route{http.MethodGet, "/identity/podget", byScope}
	opIdentityPodClear   = route{http.MethodPost, "/identity/podclear", byScope}
	opIdentityLinkStart  = route{http.MethodPost, "/identity/linkstart", byScope}

	// opPublish runs a new paste's reserve, put and confirm in one request.
	opPublish = route{http.MethodPost, "/publish/paste", singleton}

	opPasteGet        = route{http.MethodGet, "/paste/get", bySlug}
	opPasteStatus     = route{http.MethodPost, "/paste/status", bySlug}
	opPasteRename     = route{http.MethodPost, "/paste/rename", bySlug}
	opPasteRemove     = route{http.MethodPost, "/paste/remove", bySlug}
	opPasteAppend     = route{http.MethodPost, "/paste/append", bySlug}
	opPasteVersions   = route{http.MethodGet, "/paste/versions", bySlug}
	opPasteDelVersion = route{http.MethodPost, "/paste/delversion", bySlug}
	opPasteHead       = route{http.MethodGet, "/paste/head", bySlug}
	opPastePin        = route{http.MethodPost, "/paste/pin", bySlug}
	opPasteRoomIndex  = route{http.MethodGet, "/paste/roomindex", bySlug}

	opSubnetAdmit    = route{http.MethodPost, "/subnet/admit", bySubnet}
	opSubnetSnapshot = route{http.MethodGet, "/subnet/snapshot", bySubnet}

	opDenylistList = route{http.MethodGet, "/denylist/list", singleton}

	opRoomRemove = route{http.MethodPost, "/room/remove", byRoom}
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
	codePodMoved             = "pod-moved"
	codeTooManySubscriptions = "too-many-subscriptions"
	codeTooManyItems         = "too-many-items"
	reasonAbsent             = "absent"
)

// refusal is the body of a cell's refusal answer.
type refusal struct {
	Error string `json:"error"`
}

// --- publish route ----------------------------------------------------------

// publishRequest is a whole create: the row names the slug and the owner, and
// the Worker derives the reservation and its intent from it. Pod, when set, is
// the pod link id ("" for none) the row's upload id was minted for.
type publishRequest struct {
	Fingerprint string   `json:"fingerprint"`
	Generation  string   `json:"generation"`
	IntentID    string   `json:"intentId"`
	Now         int64    `json:"now"`
	Pod         *string  `json:"pod,omitempty"`
	Row         pasteRow `json:"row"`
	UserCap     int64    `json:"userCap"`
}

// --- identity cell ----------------------------------------------------------

type dropRequest struct {
	Slug string `json:"slug"`
}

type noteSubnetRequest struct {
	Addr   string `json:"addr,omitempty"`
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
	Addr     string `json:"addr,omitempty"`
	Identity string `json:"identity"`
	Limit    int    `json:"limit"`
	Now      int64  `json:"now"`
	Window   int64  `json:"window"`
}
