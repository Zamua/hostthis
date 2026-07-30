package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// KeyGateRepo backs the Sybil rate limit: it records first-seen (identity,
// ip-subnet) pairs and counts recent fresh fingerprints per subnet.
type KeyGateRepo struct {
	db *sql.DB
}

func NewKeyGateRepo(db *sql.DB) *KeyGateRepo { return &KeyGateRepo{db: db} }

// AdmitNewKey is the atomic gate. A known (identity, subnet) pair returns
// knownAlready=true with no rate-limit bookkeeping; a fresh one is admitted
// (recording its first-seen row) or rejected with ErrTooManyNewKeys once the
// subnet has limitPerSubnet fresh fingerprints inside window.
//
// The count and the insert are one BEGIN IMMEDIATE transaction, so two
// concurrent fresh keys from one subnet cannot both win the last slot.
func (r *KeyGateRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	if identity == "" || subnet == "" {
		return false, fmt.Errorf("identity + subnet required")
	}

	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var seenAt sql.NullString
	if err := tx.QueryRow(`SELECT first_seen_at FROM key_first_seen WHERE identity = ? AND ip_subnet = ?`, identity, subnet).Scan(&seenAt); err == nil {
		return true, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("lookup: %w", err)
	}

	// Fresh key: count this subnet's in-window fresh-key rows.
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

	if _, err := tx.Exec(`
		INSERT INTO key_first_seen (identity, ip_subnet, first_seen_at)
		VALUES (?, ?, ?)
	`, identity, subnet, formatTime(now)); err != nil {
		return false, fmt.Errorf("insert: %w", err)
	}
	return false, tx.Commit()
}

// SubnetSnapshot returns the fresh count and oldest first-seen across a
// subnet's in-window rows. An empty subnet yields (0, zero-time, nil).
func (r *KeyGateRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	windowStart := now.Add(-window)
	var count int
	var oldest sql.NullString
	err := r.db.QueryRow(`
		SELECT COUNT(*), MIN(first_seen_at) FROM key_first_seen
		WHERE ip_subnet = ? AND first_seen_at > ?
	`, subnet, formatTime(windowStart)).Scan(&count, &oldest)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("subnet snapshot: %w", err)
	}
	var t time.Time
	if oldest.Valid {
		t = parseTime(oldest.String)
	}
	return count, t, nil
}

// SubnetsForIdentity counts distinct in-window subnets for identity.
func (r *KeyGateRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	windowStart := now.Add(-window)
	var n int
	err := r.db.QueryRow(`
		SELECT COUNT(DISTINCT ip_subnet) FROM key_first_seen
		WHERE identity = ? AND first_seen_at > ?
	`, identity, formatTime(windowStart)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("subnets for identity: %w", err)
	}
	return n, nil
}

// DeleteFirstSeenOlderThan removes key_first_seen rows older than cutoff. Past
// the window they can never contribute to a rate-limit count again, and without
// the prune the table grows forever.
func (r *KeyGateRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	res, err := r.db.Exec(`DELETE FROM key_first_seen WHERE first_seen_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune key_first_seen: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ErrTooManyNewKeys is returned by AdmitNewKey when the subnet has hit its
// fresh-key quota for the window. Alias of the domain sentinel.
var ErrTooManyNewKeys = domain.ErrTooManyNewKeys

// txSerializable makes the modernc sqlite driver issue BEGIN IMMEDIATE: an
// exclusive write lock at transaction start, so concurrent transactions
// serialize from the very first statement.
var txSerializable = sql.TxOptions{Isolation: sql.LevelSerializable}
