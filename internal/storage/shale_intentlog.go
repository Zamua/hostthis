// The metadata-cluster implementation of durable.Log: intents are ordinary rows
// keyed by scope, so listing one scope's outstanding work is a single-shard
// prefix scan rather than a keyspace-wide fan-out.

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/Zamua/hostthis/internal/durable"
	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/cluster"
)

// ShaleIntentLog stores intents in the metadata cluster at
// intents/<scope>/<id>. The scope leads the key so it is the shard key (see
// shale_shardkey.go), which is what makes Outstanding one single-shard prefix
// scan on the shard the caller is already reading from.
//
// Nothing above storage names this type. It is reached only through
// durable.Log, so replacing it with a workflow engine is one wiring change
// (docs/SPEC.md "The durability mechanism is a port, not a layer").
type ShaleIntentLog struct {
	cluster *cluster.Cluster
	logger  *log.Logger
}

// NewShaleIntentLog is called from the same composition root that opens the
// cluster, so the log shares its connection rather than owning one.
func NewShaleIntentLog(c *cluster.Cluster, lg *log.Logger) *ShaleIntentLog {
	return &ShaleIntentLog{cluster: c, logger: lg}
}

func (r *ShaleIntentLog) log() *log.Logger {
	if r.logger != nil {
		return r.logger
	}
	return log.Default()
}

// intentRow is the persisted projection. It is a private JSON shape rather than
// durable.Intent itself so the port's type can change without a stored-format
// migration.
type intentRow struct {
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject"`
	Reached   []string  `json:"reached,omitempty"`
	Guard     []byte    `json:"guard,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

func (r *ShaleIntentLog) Begin(_ context.Context, in durable.Intent) error {
	key := shaleKeyIntent(string(in.Scope), string(in.ID))
	row := intentRow{
		Kind: string(in.Kind), Subject: in.Subject,
		Reached: stepNames(in.Reached), Guard: in.Guard, StartedAt: in.StartedAt,
	}
	return r.cluster.Transact(key, func(tx backend.Transaction) error {
		return shaleTxPutJSON(tx, key, row)
	})
}

// Advance re-reads inside the CAS rather than trusting a caller-supplied prior
// value: two steps of the same operation can be recorded concurrently only if
// the caller is buggy, but a retry recording the same step must stay a no-op.
func (r *ShaleIntentLog) Advance(_ context.Context, id durable.ID, scope durable.Scope, step durable.StepName) error {
	key := shaleKeyIntent(string(scope), string(id))
	return r.cluster.Transact(key, func(tx backend.Transaction) error {
		raw, err := tx.Get(key)
		if errors.Is(err, backend.ErrNotFound) {
			return nil // already completed; nothing to advance
		}
		if err != nil {
			return err
		}
		payload, err := stripEnvelope(raw)
		if err != nil {
			return err
		}
		var row intentRow
		if err := json.Unmarshal(payload, &row); err != nil {
			return fmt.Errorf("decode intent %s: %w", key, err)
		}
		if slices.Contains(row.Reached, string(step)) {
			return nil // idempotent
		}
		row.Reached = append(row.Reached, string(step))
		return shaleTxPutJSON(tx, key, row)
	})
}

// Complete drops the intent. An absent intent is success: two resolvers racing
// to finish the same work must both be able to return cleanly.
func (r *ShaleIntentLog) Complete(_ context.Context, id durable.ID, scope durable.Scope) error {
	err := r.cluster.Delete(shaleKeyIntent(string(scope), string(id)))
	if err != nil && errors.Is(err, backend.ErrNotFound) {
		return nil
	}
	return err
}

// Outstanding lists the scope's un-completed intents. An undecodable row is
// SKIPPED rather than failing the call: this runs inside ordinary reads, and one
// bad intent must not break a user's listing. The tolerant scan skips a
// TRUNCATED envelope (the R>1 damage shape) the same way the JSON check below
// skips a bad payload, so the fail-open claim holds for envelope-level damage
// too. The skip is logged - an intent nothing can parse is work nothing will
// ever resolve, so it needs an operator.
func (r *ShaleIntentLog) Outstanding(_ context.Context, scope durable.Scope) ([]durable.Intent, error) {
	items, err := scanPrefixTolerant(r.cluster, r.log(), shalePrefixIntents(string(scope)))
	if err != nil {
		return nil, err
	}
	out := make([]durable.Intent, 0, len(items))
	var skipped int
	for _, item := range items {
		// The tolerant scan already stripped the envelope and dropped tombstones
		// (and skipped any envelope damage), so item.Value is the payload a
		// completed intent would not have.
		var row intentRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			skipped++
			continue
		}
		out = append(out, durable.Intent{
			ID: durable.ID(extractSlug(item.Key)), Kind: durable.Kind(row.Kind),
			Scope: scope, Subject: row.Subject, Reached: stepNamesFrom(row.Reached),
			Guard: row.Guard, StartedAt: row.StartedAt,
		})
	}
	if skipped > 0 {
		r.log().Printf("shale: %d undecodable intent(s) for %s skipped; they will never resolve without an operator", skipped, scope)
	}
	sortIntentsOldestFirst(out)
	return out, nil
}

func stepNames(in []durable.StepName) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

func stepNamesFrom(in []string) []durable.StepName {
	out := make([]durable.StepName, len(in))
	for i, s := range in {
		out[i] = durable.StepName(s)
	}
	return out
}

func sortIntentsOldestFirst(in []durable.Intent) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].StartedAt.Before(in[j-1].StartedAt); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
