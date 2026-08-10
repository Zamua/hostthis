// Package durable records that a multi-step operation is in flight, so a
// process death cannot lose the knowledge that the work must be finished or
// undone.
//
// The vocabulary here is intent and resolution. There is deliberately no key,
// value, shard, or workflow in it: a backend that stores intents as rows in the
// metadata cluster and one that hands them to a workflow engine implement the
// same interface. The package imports no infrastructure so any adapter can
// depend on it without depending on ours.
//
// What this package does NOT do is interpret an intent. It stores and returns
// them; deciding what "the entry was written but the row was not" MEANS belongs
// to the component that owns those steps (docs/SPEC.md "Durable intent").
package durable

import (
	"context"
	"errors"
	"slices"
	"time"
)

// ID identifies one in-flight operation. Callers assign it, so an operation
// that is retried under the same ID resolves to the same intent rather than
// accumulating duplicates.
type ID string

// Scope is the recovery unit: the subject whose outstanding intents are listed
// together. Keeping it a single subject rather than offering a global
// "everything outstanding" is load-bearing - a global enumeration would force a
// keyspace-wide scan on a sharded store and an unbounded query on a workflow
// engine. Scoped, both are cheap.
type Scope string

// Kind names the operation, so a resolver can dispatch without guessing from
// the step list.
type Kind string

const (
	KindCreatePaste Kind = "create_paste"
	KindDeploySite  Kind = "deploy_site"

	// KindDeletePaste covers the reverse asymmetry: the authoritative rows go
	// first and the owner's enumeration entry follows on another shard, so
	// once the rows are gone nothing left identifies the owner and a retry of
	// the operation has no subject to work from.
	KindDeletePaste Kind = "delete_paste"
)

// StepName names one step of an operation. Steps are recorded as DATA, never as
// closures: resolution may run in a different process from the one that began
// the work, so what was attempted has to survive without the original stack.
type StepName string

// Intent is the durable record of an operation that started and has not been
// confirmed finished.
//
// Subject is what the operation acts on (for a paste, its slug). Guard is the
// payload a compensating delete must still find in place before removing
// anything, so cleanup cannot eat a fresh retry's work; an empty Guard means
// the step has no value to compare against.
type Intent struct {
	ID        ID
	Kind      Kind
	Scope     Scope
	Subject   string
	Reached   []StepName
	Guard     []byte
	StartedAt time.Time
}

// Reached reports whether the operation recorded that step before stopping.
func (i Intent) HasReached(s StepName) bool {
	return slices.Contains(i.Reached, s)
}

// ErrNotFound is returned when an intent is absent. Callers treat it as "already
// resolved" rather than as a failure: forgetting an intent twice is normal when
// two resolvers race.
var ErrNotFound = errors.New("durable: intent not found")

// Log durably remembers in-flight operations.
//
// Every method must be safe to call twice with the same argument. Concurrent
// resolvers are expected, and they converge only if repetition is harmless.
type Log interface {
	// Begin records the intent. Once it returns nil the intent is durable, and
	// the caller may start doing the work it describes.
	Begin(ctx context.Context, in Intent) error

	// Advance records that a step completed, so a later resolver knows how far
	// the work got. Advancing past a step already recorded is a no-op.
	Advance(ctx context.Context, id ID, scope Scope, step StepName) error

	// Complete forgets the intent: the operation finished and needs no
	// resolution. Completing an absent intent is a no-op, not an error.
	Complete(ctx context.Context, id ID, scope Scope) error

	// Outstanding returns the scope's intents that were never completed, oldest
	// first. It is the only read, and it is scoped deliberately.
	Outstanding(ctx context.Context, scope Scope) ([]Intent, error)
}
