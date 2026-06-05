package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// SignedLink is the verb-output struct for `link`.
type SignedLink struct {
	Slug    domain.Slug
	Token   string    // base64url-encoded sig + nonce + expiry
	Expires time.Time
}

// ErrInvalidShareToken is returned when a presented token doesn't
// verify against the paste's current ShareSecret + expiry.
var ErrInvalidShareToken = errors.New("service: invalid or revoked share token")

// MaxShareLinkLifetime caps how long a signed link can live. Long
// enough for "send me a link, look at it tomorrow"; short enough that
// a leaked token becomes useless quickly. The owner can always
// re-issue.
const MaxShareLinkLifetime = 7 * 24 * time.Hour

// Link issues a signed share URL for a paste. Owner-gated. The token
// is HMAC(secret, slug || expiry || nonce) — verifiable without
// per-token storage; revoked en masse by `unshare` rotating secret.
func (m *Manage) Link(slug domain.Slug, owner string, lifetime time.Duration) (SignedLink, error) {
	p, err := m.requireOwner(slug, owner)
	if err != nil {
		return SignedLink{}, err
	}
	if lifetime <= 0 {
		lifetime = 24 * time.Hour
	}
	if lifetime > MaxShareLinkLifetime {
		lifetime = MaxShareLinkLifetime
	}
	expires := m.Now().UTC().Add(lifetime)
	nonce := domain.NewShareSecret()[:8] // 8 bytes of randomness; the secret bump is the real revoker
	token := signShareToken(p.ShareSecret, slug, expires, nonce)
	return SignedLink{Slug: slug, Token: token, Expires: expires}, nil
}

// VerifyShareToken returns nil if the token is a valid signature
// against the paste's CURRENT ShareSecret and not expired. The HTTP
// layer calls this when ?k=<token> is present on an unpublished
// paste URL.
func VerifyShareToken(p domain.Paste, token string, now time.Time) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return ErrInvalidShareToken
	}
	// Layout: [32 byte MAC][8 byte nonce][8 byte unix-nano expiry]
	if len(raw) != 32+8+8 {
		return ErrInvalidShareToken
	}
	mac := raw[:32]
	nonce := raw[32:40]
	expiryNs := int64(binary.BigEndian.Uint64(raw[40:48])) //nolint:gosec // fixed-width 8-byte slice
	expires := time.Unix(0, expiryNs).UTC()
	if !now.UTC().Before(expires) {
		return ErrInvalidShareToken
	}
	expected := computeMAC(p.ShareSecret, p.Slug, expires, nonce)
	if !hmac.Equal(mac, expected) {
		return ErrInvalidShareToken
	}
	return nil
}

func signShareToken(secret []byte, slug domain.Slug, expires time.Time, nonce []byte) string {
	mac := computeMAC(secret, slug, expires, nonce)
	out := make([]byte, 0, 48)
	out = append(out, mac...)
	out = append(out, nonce...)
	out = binary.BigEndian.AppendUint64(out, uint64(expires.UnixNano())) //nolint:gosec // positive future time
	return base64.RawURLEncoding.EncodeToString(out)
}

func computeMAC(secret []byte, slug domain.Slug, expires time.Time, nonce []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(slug.String()))
	h.Write([]byte{0}) // delimiter — paranoia against ambiguity attacks
	h.Write([]byte(strconv.FormatInt(expires.UnixNano(), 10)))
	h.Write([]byte{0})
	h.Write(nonce)
	return h.Sum(nil)
}
