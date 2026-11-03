package abuselog

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

func capture() (*Logger, *[]string) {
	var lines []string
	return New(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }), &lines
}

// Port-shaped guard: the address is a parameter only so it can be truncated;
// no header, body, URL or user agent can be handed to the line.
var _ func(*Logger, domain.AbuseEvent, string, int, net.IP) = (*Logger).Log

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
		logger.Log(domain.AbuseRateLimited, "collection", 429, ip)
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
	l.Log(domain.AbuseCreated, "collection", 201, net.IPv4(1, 2, 3, 4))
	New(nil).Log(domain.AbuseCreated, "collection", 201, net.IPv4(1, 2, 3, 4))
}
