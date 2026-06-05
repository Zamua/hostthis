package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// TokenRepo persists HTTP API tokens. The plaintext token is NEVER
// stored — we hash with sha256 at insert time and check against the
// hash at auth time.
type TokenRepo struct {
	db *sql.DB
}

func NewTokenRepo(db *sql.DB) *TokenRepo { return &TokenRepo{db: db} }

// InsertToken records the hash of a token issued to owner. prefix is
// the first 8 chars of the raw token (for UI / `token list`); it
// alone is NOT secret-bearing.
func (r *TokenRepo) InsertToken(ownerHash, tokenSha256, prefix string, now time.Time) error {
	_, err := r.db.Exec(`
		INSERT INTO api_tokens (token_sha256, owner_hash, prefix, created_at)
		VALUES (?, ?, ?, ?)
	`, tokenSha256, ownerHash, prefix, formatTime(now))
	if err != nil {
		return fmt.Errorf("insert token: %w", err)
	}
	return nil
}

// LookupTokenOwner returns the owner_hash for a presented token's
// sha256. ErrNotFound for unknown tokens.
func (r *TokenRepo) LookupTokenOwner(tokenSha256 string) (string, error) {
	var owner string
	err := r.db.QueryRow(`SELECT owner_hash FROM api_tokens WHERE token_sha256 = ?`, tokenSha256).Scan(&owner)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("lookup token: %w", err)
	}
	return owner, nil
}

// TouchToken updates last_used_at on each successful auth so users
// can prune stale tokens.
func (r *TokenRepo) TouchToken(tokenSha256 string, now time.Time) error {
	_, err := r.db.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE token_sha256 = ?`, formatTime(now), tokenSha256)
	return err
}

// ListTokens returns metadata (NOT secrets) for the owner's tokens.
func (r *TokenRepo) ListTokens(ownerHash string) ([]TokenInfo, error) {
	if ownerHash == "" {
		return nil, nil
	}
	rows, err := r.db.Query(`
		SELECT prefix, created_at, last_used_at
		FROM api_tokens WHERE owner_hash = ?
		ORDER BY created_at DESC
	`, ownerHash)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		var created string
		var lastUsed sql.NullString
		if err := rows.Scan(&t.Prefix, &created, &lastUsed); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(created)
		if lastUsed.Valid {
			t.LastUsedAt = parseTime(lastUsed.String)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeByPrefix deletes any token whose prefix matches AND which
// belongs to the owner. Prefix is 8 chars so the probability of
// accidentally hitting more than one is small.
func (r *TokenRepo) RevokeByPrefix(ownerHash, prefix string) (int, error) {
	res, err := r.db.Exec(`DELETE FROM api_tokens WHERE owner_hash = ? AND prefix = ?`, ownerHash, prefix)
	if err != nil {
		return 0, fmt.Errorf("revoke token: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// TokenInfo is the non-secret view of a token row.
type TokenInfo struct {
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero if never used
}

// Ensure domain stays decoupled from this file (no domain type
// leakage). We don't need to reference domain at all here.
var _ = domain.HashContent
