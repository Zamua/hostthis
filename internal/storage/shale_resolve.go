// Resolving durable intents: deciding whether a half-finished write should be
// completed or undone, and the boot sweep that drives it.

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
)

// Steps an insert records. Only the ones a resolver needs to distinguish are
// named: what matters is whether the ENTRY was written, because that is the
// thing a rollback removes.
const (
	StepEntryWritten durable.StepName = "entry"
)

// ResolveGrace is how old an intent must be before a sweep may act on it.
//
// It is not a tuning knob, it is a correctness requirement. Which pod HANDLES a
// request is chosen by the load balancer; which pod STORES that request's
// intent is chosen by hashing the owner. The two are unrelated, so a node's own
// local intents are routinely created by uploads other nodes are handling right
// now - and an in-flight intent is indistinguishable from an abandoned one.
//
// The value guard cannot separate them: a live upload's entry MATCHES the
// intent describing it, so a guarded delete would fire against a running
// request. Only elapsed time distinguishes the two, so this MUST exceed the
// longest plausible upload (the same contract DefaultOrphanGrace carries for
// the blob plane). A var so tests can shrink it.
var ResolveGrace = 10 * time.Minute

// ResolveIntents resolves the intents this node can see that are older than the
// grace, and reports how many it settled.
//
// Safe to run concurrently with other nodes and with live traffic: each step is
// idempotent, the compensating delete is value-guarded, and an intent younger
// than the grace is left alone. There are no locks.
//
// One bad intent does not stop the sweep. This runs at boot on a serving node,
// so a single unresolvable record must not deny the whole sweep to every other.
func (r *ShaleRepo) ResolveIntents(ctx context.Context, intents []durable.Intent, now time.Time) (int, error) {
	var settled int
	var firstErr error
	for _, in := range intents {
		if now.Sub(in.StartedAt) < ResolveGrace {
			continue // may still be in flight on another node
		}
		if err := r.resolveIntent(ctx, in); err != nil {
			r.repoLog().Printf("shale: resolving intent %s (%s %s): %v", in.ID, in.Kind, in.Subject, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		settled++
	}
	return settled, firstErr
}

// resolveIntent settles one intent by looking at the authoritative row, never by
// inferring from how far the intent got:
//
//	row present -> the write SUCCEEDED and only the bookkeeping was lost. Roll
//	               FORWARD: forget the intent, touch nothing else. Rolling back
//	               here would delete a live paste.
//	row absent  -> the write never landed. Roll BACK: drop the enumeration
//	               entry, then forget the intent.
//
// The order matters on the rollback path. The entry is dropped BEFORE the
// intent is forgotten, so a crash mid-resolve leaves the intent in place and
// the next sweep repeats the work. Forgetting first would strand the entry as a
// phantom with nothing left to describe it.
func (r *ShaleRepo) resolveIntent(ctx context.Context, in durable.Intent) error {
	if in.Kind != durable.KindCreatePaste && in.Kind != durable.KindDeploySite {
		// An intent this build does not understand. Leave it: a newer build may
		// own it, and deleting work we cannot interpret is how data disappears.
		return nil
	}
	present, err := r.subjectRowExists(in)
	if err != nil {
		return err
	}
	if !present && in.HasReached(StepEntryWritten) {
		if _, err := r.guardedDeleteIndexEntry(r.intentEntryKey(in), in.Guard); err != nil {
			return err
		}
	}
	return r.intents.Complete(ctx, in.ID, in.Scope)
}

// subjectRowExists reports whether the authoritative row the intent describes
// is present. A decode failure counts as PRESENT: the row exists, it is merely
// unreadable, and the one thing a resolver must never do is roll back over a
// record it could not read.
func (r *ShaleRepo) subjectRowExists(in durable.Intent) (bool, error) {
	raw, err := r.getRaw(shaleKeyPaste(domain.Slug(in.Subject)))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if len(raw) == 0 {
		return false, nil
	}
	var probe json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return true, nil // unreadable, but it is there
	}
	return true, nil
}

func (r *ShaleRepo) intentEntryKey(in durable.Intent) []byte {
	return shaleKeyIdentityPaste(string(in.Scope), in.Subject)
}

// SweepIntents is the boot sweep: it resolves every intent stored on the units
// THIS node has mounted.
//
// The scan is node-LOCAL. cluster.LocalScanPrefix walks this node's own mounted
// units, so no network fan-out happens even though the intent family logically
// spans every shard. Across the fleet every unit is covered, because every unit
// is mounted by someone.
//
// Call it after the node is serving, not before. Deciding an intent requires
// reading the authoritative row, which lives on a DIFFERENT shard that may not
// be mounted anywhere yet during a cold start - gating readiness on that read
// would deadlock a cold cluster.
func (r *ShaleRepo) SweepIntents(ctx context.Context, now time.Time) (int, error) {
	// Retried on ErrAcquiring with a boot-sized budget: this runs while the
	// node's own positions may still be acquiring, and that refusal resolves on
	// its own. Without the retry the sweep fails on every boot, which is
	// indistinguishable from a feature that does not exist.
	var found []durable.Intent
	err := retryAcquiring(bootRetry, r.repoLog(), "intent-sweep", func() error {
		found = nil
		it, err := r.cluster.LocalScanPrefix(prefixIntentsAll)
		if err != nil {
			return err
		}
		defer it.Close() //nolint:errcheck
		for {
			k, v, err := it.Next()
			if err != nil {
				return err
			}
			if k == nil && v == nil {
				return nil
			}
			in, ok := decodeLocalIntent(k, v)
			if !ok {
				continue // tombstone or unreadable; the log's own scan reports those
			}
			found = append(found, in)
		}
	})
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, nil
	}
	settled, err := r.ResolveIntents(ctx, found, now)
	r.repoLog().Printf("shale: intent sweep: %d found on mounted units, %d settled (older than %s)", len(found), settled, ResolveGrace)
	return settled, err
}

// decodeLocalIntent reads one raw scanned record. LocalScanPrefix returns the
// stored bytes, so the envelope has to come off here - unlike scanPrefixOn,
// which strips it already.
func decodeLocalIntent(k, v []byte) (durable.Intent, bool) {
	payload, err := stripEnvelope(v)
	if err != nil || len(payload) == 0 {
		return durable.Intent{}, false
	}
	var row intentRow
	if json.Unmarshal(payload, &row) != nil {
		return durable.Intent{}, false
	}
	scope, id, ok := splitIntentKey(k)
	if !ok {
		return durable.Intent{}, false
	}
	return durable.Intent{
		ID: durable.ID(id), Kind: durable.Kind(row.Kind), Scope: durable.Scope(scope),
		Subject: row.Subject, Reached: stepNamesFrom(row.Reached),
		Guard: row.Guard, StartedAt: row.StartedAt,
	}, true
}

// splitIntentKey pulls scope and id back out of intents/<scope>/<id>. The scope
// itself contains no slash (it is an identity), so one split is enough.
func splitIntentKey(k []byte) (scope, id string, ok bool) {
	rest, found := bytes.CutPrefix(k, prefixIntentsAll)
	if !found {
		return "", "", false
	}
	s, i, found := bytes.Cut(rest, []byte("/"))
	if !found || len(s) == 0 || len(i) == 0 {
		return "", "", false
	}
	return string(s), string(i), true
}

// resolveOwnerIntents settles the owner's outstanding intents opportunistically,
// riding a read that is already talking to their {id} shard.
//
// This is why a phantom does not have to wait for a pod restart. The boot sweep
// remains the guarantee - it covers owners who never come back - but in practice
// the owner's own next listing clears their own residue.
//
// BEST-EFFORT: a failure here is logged and swallowed. The caller is serving a
// user's read, and a resolver problem must never turn that into an error. Cost
// in the normal case is one prefix scan on a shard the caller is already
// reading, returning nothing.
func (r *ShaleRepo) resolveOwnerIntents(ctx context.Context, owner string, now time.Time) {
	if owner == "" {
		return
	}
	out, err := r.intents.Outstanding(ctx, durable.Scope(owner))
	if err != nil {
		r.repoLog().Printf("shale: listing outstanding intents for %s: %v (the boot sweep still covers them)", owner, err)
		return
	}
	if len(out) == 0 {
		return
	}
	settled, err := r.ResolveIntents(ctx, out, now)
	if err != nil {
		r.repoLog().Printf("shale: resolve-on-read for %s: %v (the boot sweep still covers them)", owner, err)
	}
	if settled > 0 {
		r.repoLog().Printf("shale: resolve-on-read settled %d half-finished write(s) for %s", settled, owner)
	}
}
