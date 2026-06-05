package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// TokenRepo persists API token hashes. internal/storage.TokenRepo
// satisfies it.
type TokenRepo interface {
	InsertToken(ownerHash, tokenSha256, prefix string, now time.Time) error
	LookupTokenOwner(tokenSha256 string) (string, error)
	TouchToken(tokenSha256 string, now time.Time) error
}

// TokenService issues + verifies API bearer tokens.
type TokenService struct {
	Repo TokenRepo
	Now  func() time.Time
}

func NewTokenService(repo TokenRepo) *TokenService {
	return &TokenService{Repo: repo, Now: time.Now}
}

// Create generates a fresh token, persists only its hash, and
// returns the raw token to the caller exactly once. The caller
// echoes it to the user; the user puts it in `Authorization: Bearer`.
//
// Token shape: `htst_live_<48-char hex>`. The prefix is meaningful
// for the user (clearly a hostthis token) and the hex tail is 24
// bytes of crypto/rand entropy.
func (t *TokenService) Create(ownerHash string) (string, error) {
	if ownerHash == "" {
		return "", ErrEmptyOwner
	}
	raw, err := generateRawToken()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	prefix := raw[:tokenPrefixLen]
	if err := t.Repo.InsertToken(ownerHash, hash, prefix, t.Now().UTC()); err != nil {
		return "", err
	}
	return raw, nil
}

// Authenticate maps a presented raw token to its owner, updating
// last_used_at on success. Returns ErrInvalidToken for unknown tokens.
func (t *TokenService) Authenticate(rawToken string) (string, error) {
	sum := sha256.Sum256([]byte(rawToken))
	hash := hex.EncodeToString(sum[:])
	owner, err := t.Repo.LookupTokenOwner(hash)
	if err != nil {
		return "", ErrInvalidToken
	}
	_ = t.Repo.TouchToken(hash, t.Now().UTC()) // best-effort
	return owner, nil
}

// ErrInvalidToken is returned for unknown / malformed bearer tokens.
var ErrInvalidToken = errors.New("service: invalid api token")

const (
	tokenPrefixLen = len("htst_live_") + 8 // include the literal prefix in the displayed prefix
	tokenEntropy   = 24                    // bytes
)

func generateRawToken() (string, error) {
	buf := make([]byte, tokenEntropy)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "htst_live_" + hex.EncodeToString(buf), nil
}
