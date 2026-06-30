package domain

import (
	"testing"
	"time"
)

func TestRetentionExpiryFor(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	t.Run("positive window stamps now+window", func(t *testing.T) {
		r := Retention{Window: 30 * 24 * time.Hour}
		got := r.ExpiryFor(now)
		want := now.Add(30 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Fatalf("ExpiryFor = %v, want %v", got, want)
		}
		if !r.Enabled() {
			t.Fatalf("Enabled() = false, want true")
		}
	})

	for _, w := range []time.Duration{0, -time.Hour} {
		t.Run("non-positive window never expires", func(t *testing.T) {
			r := Retention{Window: w}
			if r.Enabled() {
				t.Fatalf("Enabled() = true for window %v, want false", w)
			}
			got := r.ExpiryFor(now)
			if !got.Equal(NeverExpires) {
				t.Fatalf("ExpiryFor = %v, want NeverExpires %v", got, NeverExpires)
			}
			// The sentinel must sort AFTER any plausible 'now' so the sweep's
			// `expires_at < now` never matches it.
			if !NeverExpires.After(now.AddDate(500, 0, 0)) {
				t.Fatalf("NeverExpires %v is not far enough in the future", NeverExpires)
			}
		})
	}
}

func TestRetentionDescribe(t *testing.T) {
	cases := []struct {
		window time.Duration
		want   string
	}{
		{30 * 24 * time.Hour, "30 days"},
		{24 * time.Hour, "1 day"},
		{12 * time.Hour, "12 hours"},
		{time.Hour, "1 hour"},
		{90 * time.Minute, "90 minutes"},
		{time.Minute, "1 minute"},
		{0, "never"},
		{-5 * time.Hour, "never"},
	}
	for _, c := range cases {
		got := Retention{Window: c.window}.Describe()
		if got != c.want {
			t.Errorf("Describe(%v) = %q, want %q", c.window, got, c.want)
		}
	}
}

func TestDefaultRetention(t *testing.T) {
	r := DefaultRetention()
	if r.Window != DefaultRetentionWindow {
		t.Fatalf("DefaultRetention().Window = %v, want %v", r.Window, DefaultRetentionWindow)
	}
	if r.Describe() != "30 days" {
		t.Fatalf("DefaultRetention().Describe() = %q, want %q", r.Describe(), "30 days")
	}
}
