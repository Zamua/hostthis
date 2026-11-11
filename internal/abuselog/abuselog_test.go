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

// A line carries the event, route and status, the client's address in its
// canonical text, and the subnet that address groups under.
func TestLineCarriesTheAddressAndItsSubnet(t *testing.T) {
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
		if len(fields) != 5 || fields["event"] != "rate-limited" || fields["route"] != "collection" || fields["status"] != float64(429) {
			t.Fatalf("fields %v in %q", fields, line)
		}
		want, _ := netip.AddrFromSlice(ip)
		want = want.Unmap()
		if fields["addr"] != want.String() {
			t.Fatalf("addr %q, want %q", fields["addr"], want)
		}
		prefix, err := netip.ParsePrefix(fields["subnet"].(string))
		if err != nil {
			t.Fatalf("subnet %q: %v", fields["subnet"], err)
		}
		if prefix.Bits() != bits || prefix.Addr() != netip.PrefixFrom(want, bits).Masked().Addr() {
			t.Fatalf("%v grouped under %v, want %d kept bits", ip, prefix, bits)
		}
	}
	logger, lines := capture()
	logger.Log(domain.AbuseRateLimited, "ssh", 6, nil)
	if !strings.Contains((*lines)[0], `"addr":"unknown"`) {
		t.Fatalf("a missing address logged as %q", (*lines)[0])
	}
}

func TestNilLoggerIsANoOp(t *testing.T) {
	var l *Logger
	l.Log(domain.AbuseCreated, "collection", 201, net.IPv4(1, 2, 3, 4))
	New(nil).Log(domain.AbuseCreated, "collection", 201, net.IPv4(1, 2, 3, 4))
}
