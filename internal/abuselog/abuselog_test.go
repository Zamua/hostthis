package abuselog

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"
)

func capture() (*Logger, *[]string) {
	var lines []string
	return New(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }), &lines
}

// Port-shaped guard: the address is a parameter only so it can be truncated;
// no header, body, URL or user agent can be handed to the line.
var _ func(*Logger, Event, string, int, net.IP) = (*Logger).Log

func TestSubnetVectorsMatchTheWorker(t *testing.T) {
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
		if got := Subnet(net.ParseIP(v.IP)); got != v.Subnet {
			t.Errorf("Subnet(%q) = %q, want %q", v.IP, got, v.Subnet)
		}
	}
}

func TestSubnetOfOddInputsIsUnknown(t *testing.T) {
	for _, ip := range []net.IP{nil, {}, {1, 2, 3}, make(net.IP, 5), make(net.IP, 17)} {
		if got := Subnet(ip); got != "unknown" {
			t.Errorf("Subnet(%v) = %q", []byte(ip), got)
		}
	}
	if got := Subnet(net.IPv4(203, 0, 113, 77)); got != "203.0.113.0/24" {
		t.Errorf("4-in-16 form: %q", got)
	}
	if got := Subnet(net.IPv4(203, 0, 113, 77).To4()); got != "203.0.113.0/24" {
		t.Errorf("4-byte form: %q", got)
	}
}

func TestLineCarriesFourFieldsAndNeverTheAddress(t *testing.T) {
	rng := rand.New(rand.NewSource(20261103))
	for i := range 1000 {
		var ip net.IP
		var bits int
		if i%2 == 0 {
			ip = net.IPv4(byte(1+rng.Intn(255)), byte(rng.Intn(256)), byte(rng.Intn(256)), byte(1+rng.Intn(255)))
			bits = 24
		} else {
			ip = make(net.IP, 16)
			rng.Read(ip)
			ip[5] |= 1    // nonzero host bits just past bit 40
			ip[15] |= 1   // and in the last byte
			ip[0] |= 0x20 // keeps clear of the IPv4-mapped range
			bits = 40
		}
		logger, lines := capture()
		logger.Log(RateLimited, "collection", 429, ip)
		if len(*lines) != 1 {
			t.Fatalf("got %d lines", len(*lines))
		}
		line := (*lines)[0]
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		if len(keys) != 4 || fields["event"] != "rate-limited" || fields["route"] != "collection" || fields["status"] != float64(429) {
			t.Fatalf("fields %v in %q", keys, line)
		}
		for _, spelling := range spellings(ip) {
			if strings.Contains(line, spelling) {
				t.Fatalf("line %q holds the address %q", line, spelling)
			}
		}
		prefix, err := netip.ParsePrefix(fields["subnet"].(string))
		if err != nil {
			t.Fatalf("subnet %q: %v", fields["subnet"], err)
		}
		want, _ := netip.AddrFromSlice(ip)
		want = want.Unmap()
		if prefix.Bits() != bits || prefix.Addr() != netip.PrefixFrom(want, bits).Masked().Addr() {
			t.Fatalf("%v logged as %v, want %d kept bits", ip, prefix, bits)
		}
		if prefix.Addr() != prefix.Masked().Addr() {
			t.Fatalf("host bits survive in %v", prefix)
		}
	}
}

// spellings are the textual forms an address could reach a line in.
func spellings(ip net.IP) []string {
	out := []string{ip.String()}
	if a, ok := netip.AddrFromSlice(ip); ok {
		out = append(out, a.Unmap().String(), strings.ToUpper(a.String()))
		if a.Is6() && !a.Is4In6() {
			out = append(out, a.StringExpanded())
		}
	}
	return out
}

func TestNilLoggerIsANoOp(t *testing.T) {
	var l *Logger
	l.Log(Created, "collection", 201, net.IPv4(1, 2, 3, 4))
	New(nil).Log(Created, "collection", 201, net.IPv4(1, 2, 3, 4))
}

func TestEventNamesAreTheWorkers(t *testing.T) {
	got := []Event{Created, RateLimited, Capacity, Oversize, Rejected, AuthFailure}
	want := []string{"created", "rate-limited", "capacity", "oversize", "rejected", "auth-failure"}
	var names []string
	for _, e := range got {
		names = append(names, string(e))
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("%v", names)
	}
}
