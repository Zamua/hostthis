package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KeyGateRepo backs the Sybil rate limit — tracks first-seen pairs
// of (identity, ip-subnet) and counts how many fresh fingerprints
// have come from a subnet recently.
type KeyGateRepo struct {
	db *sql.DB
}

func NewKeyGateRepo(db *sql.DB) *KeyGateRepo { return &KeyGateRepo{db: db} }

// AdmitNewKey is the atomic gate: in a single BEGIN IMMEDIATE
// transaction it checks whether (identity, subnet) is already known
// (returning success immediately if so), or, if it's new, counts how
// many distinct fresh fingerprints have appeared from this subnet in
// the last `window` and either admits (recording the first-seen row)
// or rejects.
//
// Returns:
//   - knownAlready=true,  err=nil  → returning user, no rate-limit
//     bookkeeping needed
//   - knownAlready=false, err=nil  → new fingerprint admitted, row
//     recorded
//   - knownAlready=false, err=ErrTooManyNewKeys → rate-limited
//     (caller surfaces this to the user)
//
// The check + insert is one transaction so two concurrent fresh keys
// from the same subnet can't both win the last slot.
func (r *KeyGateRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	if identity == "" || subnet == "" {
		return false, fmt.Errorf("identity + subnet required")
	}

	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Already known? Fast path — no rate-limit accounting.
	var seenAt sql.NullString
	if err := tx.QueryRow(`SELECT first_seen_at FROM key_first_seen WHERE identity = ? AND ip_subnet = ?`, identity, subnet).Scan(&seenAt); err == nil {
		return true, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("lookup: %w", err)
	}

	// Fresh key — count current fresh-key rows from this subnet
	// within the window.
	windowStart := now.Add(-window)
	var freshCount int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM key_first_seen
		WHERE ip_subnet = ? AND first_seen_at > ?
	`, subnet, formatTime(windowStart)).Scan(&freshCount); err != nil {
		return false, fmt.Errorf("count fresh: %w", err)
	}
	if freshCount >= limitPerSubnet {
		return false, ErrTooManyNewKeys
	}

	// Admit + record.
	if _, err := tx.Exec(`
		INSERT INTO key_first_seen (identity, ip_subnet, first_seen_at)
		VALUES (?, ?, ?)
	`, identity, subnet, formatTime(now)); err != nil {
		return false, fmt.Errorf("insert: %w", err)
	}
	return false, tx.Commit()
}

// DeleteFirstSeenOlderThan removes key_first_seen rows whose
// first_seen_at is before cutoff. Such rows can never contribute to
// the rate-limit count again — they're past the window — so they're
// safe to drop. Without this, the table grows forever.
func (r *KeyGateRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	res, err := r.db.Exec(`DELETE FROM key_first_seen WHERE first_seen_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune key_first_seen: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ErrTooManyNewKeys is returned by AdmitNewKey when the subnet has
// hit its fresh-key quota for the window.
var ErrTooManyNewKeys = errors.New("storage: too many new keys from this network")

// txSerializable asks the modernc sqlite driver to issue
// BEGIN IMMEDIATE — exclusive write lock at transaction start so
// concurrent transactions serialize from the very first statement.
var txSerializable = sql.TxOptions{Isolation: sql.LevelSerializable}
