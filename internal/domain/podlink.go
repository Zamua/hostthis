package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// PodLinkTTL is how long a link URL stays usable.
const PodLinkTTL = 15 * time.Minute

const podUploadPrefix = "pod_"

// ErrPodsUnavailable reports a deployment without pod support.
var ErrPodsUnavailable = errors.New("pods are not available on this server")

// PodRefusedError is a pod's refusal of a request hostthis made to it; Status
// is its answer, or 0 when it could not be reached.
type PodRefusedError struct{ Status int }

func (e *PodRefusedError) Error() string {
	if e.Status == 0 {
		return "your pod could not be reached; check that its hostthis/ folder is still shared with hostthis, or run pod link again"
	}
	return fmt.Sprintf("your pod refused the write (%d); check that its hostthis/ folder is still shared with hostthis, or run pod link again", e.Status)
}

// PodLink is an owner's linked pod.
type PodLink struct {
	ID      string
	WebID   string
	Storage string
	Linked  time.Time
}

func randomHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic("hostthis: crypto/rand failure: " + err.Error())
	}
	return hex.EncodeToString(raw)
}

// NewPodLinkID returns a random link id: 16 lowercase hex digits.
func NewPodLinkID() string { return randomHex(8) }

// NewPodLinkSecret returns the secret a link URL carries.
func NewPodLinkSecret() string { return randomHex(32) }

// PodLinkSecretHash is the form the Worker keeps a link secret in.
func PodLinkSecretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func ValidPodLinkID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NewPodUploadID is an upload id whose objects the Worker stores in the pod of
// link (docs/SPEC.md "Pod storage").
func NewPodUploadID(link string) string {
	return podUploadPrefix + link + "_" + NewUploadID()
}

// PodLinkOfUploadID is the link a pod upload id names.
func PodLinkOfUploadID(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, podUploadPrefix)
	if !ok {
		return "", false
	}
	link, _, ok := strings.Cut(rest, "_")
	if !ok || !ValidPodLinkID(link) {
		return "", false
	}
	return link, true
}

// ParseWebID accepts an absolute https URL, or http on localhost, with no
// whitespace or control characters.
func ParseWebID(raw string) (string, error) {
	bad := fmt.Errorf("not a WebID: %q (want https://...)", raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", bad
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", bad
	}
	localhost := u.Hostname() == "localhost" || strings.HasSuffix(u.Hostname(), ".localhost")
	if u.Scheme != "https" && (u.Scheme != "http" || !localhost) {
		return "", bad
	}
	return raw, nil
}
