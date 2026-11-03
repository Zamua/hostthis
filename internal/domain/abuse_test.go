package domain

import (
	"encoding/json"
	"net"
	"os"
	"testing"
)

// The vectors in testdata/abuse-subnet-vectors.json, which the Worker's
// abuse-log.test.js runs too, so the two truncations cannot drift.
func TestAbuseSubnetVectorsMatchTheWorker(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/abuse-subnet-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ IP, Subnet string }
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 10 {
		t.Fatalf("only %d vectors", len(vectors))
	}
	for _, v := range vectors {
		if got := AbuseSubnet(net.ParseIP(v.IP)); got != v.Subnet {
			t.Errorf("AbuseSubnet(%q) = %q, want %q", v.IP, got, v.Subnet)
		}
	}
}

func TestAbuseSubnetOddInputsAreUnknown(t *testing.T) {
	for _, ip := range []net.IP{nil, {}, {1, 2, 3}, make(net.IP, 5), make(net.IP, 17)} {
		if got := AbuseSubnet(ip); got != "unknown" {
			t.Errorf("AbuseSubnet(%v) = %q", []byte(ip), got)
		}
	}
	if got := AbuseSubnet(net.IPv4(203, 0, 113, 77)); got != "203.0.113.0/24" {
		t.Errorf("4-in-16 form: %q", got)
	}
	if got := AbuseSubnet(net.IPv4(203, 0, 113, 77).To4()); got != "203.0.113.0/24" {
		t.Errorf("4-byte form: %q", got)
	}
}

func TestAbuseEventNamesAreTheWorkers(t *testing.T) {
	got := []AbuseEvent{AbuseCreated, AbuseRateLimited, AbuseCapacity, AbuseOversize, AbuseRejected, AbuseAuthFailure}
	want := []string{"created", "rate-limited", "capacity", "oversize", "rejected", "auth-failure"}
	for i, e := range got {
		if string(e) != want[i] {
			t.Errorf("event %d = %q, want %q", i, e, want[i])
		}
	}
}
