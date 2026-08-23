// Package celld adapts hostthis's storage ports to a celld fleet.
//
// celld serves HTTP and WebSocket only, so hostthis stays a Go process and
// becomes a CLIENT of the fleet. That introduces a network boundary where the
// shale adapter has an in-process call, which is the cost of the experiment
// rather than an accident of it.
//
// Nothing above internal/storage names this package: it is reached through the
// same ports the shale adapter satisfies, so swapping backends is a wiring
// change (docs/design/celld-port-audit.md).
package celld

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Zamua/hostthis/internal/durable"
)

// IntentLog is the celld implementation of durable.Log.
//
// One cell per scope, addressed by name, so a scope's intents are a single
// entity's private storage and Outstanding is a read of one cell rather than a
// scan. The scope-bounded property the KV backend maintains through key layout
// is structural here: a cell cannot see another cell's storage.
type IntentLog struct {
	base   string
	client *http.Client
}

// NewIntentLog points the adapter at a fleet's public listener. base is the
// origin only, e.g. http://celld.hostthis-staging.svc.cluster.local:8080.
func NewIntentLog(base string, c *http.Client) *IntentLog {
	if c == nil {
		// A metadata call must not hang a user's SSH session indefinitely; the
		// caller's context still bounds it, this is the backstop.
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &IntentLog{base: base, client: c}
}

// wireIntent is the HTTP shape. Private so the port's type can change without
// a wire migration, and Guard is base64 because JSON cannot carry raw bytes.
type wireIntent struct {
	ID        string   `json:"id"`
	Scope     string   `json:"scope"`
	Kind      string   `json:"kind"`
	Subject   string   `json:"subject"`
	Reached   []string `json:"reached"`
	Guard     string   `json:"guard"`
	StartedAt int64    `json:"startedAt"`
	Step      string   `json:"step,omitempty"`
}

func (r *IntentLog) do(ctx context.Context, method, op, scope string, body any, out any) error {
	u := fmt.Sprintf("%s/intents/%s?scope=%s", r.base, op, url.QueryEscape(scope))
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("celld: encode %s: %w", op, err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return fmt.Errorf("celld: build %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("celld: %s: %w", op, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		return fmt.Errorf("celld: %s: unexpected status %d", op, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("celld: decode %s: %w", op, err)
	}
	return nil
}

// Begin records the intent. Keyed by the caller's id, so a retried operation
// converges on one intent instead of accumulating duplicates.
func (r *IntentLog) Begin(ctx context.Context, in durable.Intent) error {
	return r.do(ctx, http.MethodPost, "begin", string(in.Scope), wireIntent{
		ID:        string(in.ID),
		Scope:     string(in.Scope),
		Kind:      string(in.Kind),
		Subject:   in.Subject,
		Reached:   stepStrings(in.Reached),
		Guard:     base64.StdEncoding.EncodeToString(in.Guard),
		StartedAt: in.StartedAt.UTC().UnixMilli(),
	}, nil)
}

// Advance records a step. Advancing past a recorded step, or advancing an
// intent another resolver already completed, is a no-op rather than an error.
func (r *IntentLog) Advance(ctx context.Context, id durable.ID, scope durable.Scope, step durable.StepName) error {
	return r.do(ctx, http.MethodPost, "advance", string(scope), wireIntent{
		ID:    string(id),
		Scope: string(scope),
		Step:  string(step),
	}, nil)
}

// Complete forgets the intent. Completing an absent one is normal when two
// resolvers race.
func (r *IntentLog) Complete(ctx context.Context, id durable.ID, scope durable.Scope) error {
	return r.do(ctx, http.MethodPost, "complete", string(scope), wireIntent{
		ID:    string(id),
		Scope: string(scope),
	}, nil)
}

// Outstanding lists the scope's un-completed intents, oldest first.
func (r *IntentLog) Outstanding(ctx context.Context, scope durable.Scope) ([]durable.Intent, error) {
	var wire []wireIntent
	if err := r.do(ctx, http.MethodGet, "outstanding", string(scope), nil, &wire); err != nil {
		return nil, err
	}
	out := make([]durable.Intent, 0, len(wire))
	for _, w := range wire {
		guard, err := base64.StdEncoding.DecodeString(w.Guard)
		if err != nil {
			// Fail open on one unreadable record rather than deny the whole
			// scope its recovery listing (CLAUDE.md engineering principle 3).
			continue
		}
		out = append(out, durable.Intent{
			ID:        durable.ID(w.ID),
			Kind:      durable.Kind(w.Kind),
			Scope:     scope,
			Subject:   w.Subject,
			Reached:   stepNames(w.Reached),
			Guard:     guard,
			StartedAt: time.UnixMilli(w.StartedAt).UTC(),
		})
	}
	return out, nil
}

func stepStrings(in []durable.StepName) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

func stepNames(in []string) []durable.StepName {
	out := make([]durable.StepName, len(in))
	for i, s := range in {
		out[i] = durable.StepName(s)
	}
	return out
}

var _ durable.Log = (*IntentLog)(nil)
