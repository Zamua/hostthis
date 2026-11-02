package service

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

type scriptedDenySource struct {
	entries []domain.DenyEntry
	err     error
}

func (s *scriptedDenySource) Denylist(context.Context) ([]domain.DenyEntry, error) {
	return s.entries, s.err
}

var (
	gateKey   = "SHA256:" + strings.Repeat("ab", 32)
	gateOwner = mustOwner(gateKey)
	gateAddr  = netip.MustParseAddr("203.0.113.5")
)

func mustOwner(fp string) domain.Owner {
	o, err := domain.OwnerFromKeyFingerprint(fp)
	if err != nil {
		panic(err)
	}
	return o
}

func TestDenyGate_AdmitsEveryoneBeforeTheFirstRefresh(t *testing.T) {
	g := NewDenyGate(&scriptedDenySource{entries: []domain.DenyEntry{{Entry: gateKey}}})
	if _, banned := g.Check(gateOwner, gateAddr); banned {
		t.Fatal("a session was refused before any list was loaded")
	}
}

func TestDenyGate_RefreshLoadsTheList(t *testing.T) {
	g := NewDenyGate(&scriptedDenySource{entries: []domain.DenyEntry{{Entry: gateKey, Reason: "abuse"}}})
	if err := g.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reason, banned := g.Check(gateOwner, gateAddr); !banned || reason != "abuse" {
		t.Fatalf("Check = %q, %v; want the loaded entry", reason, banned)
	}
}

func TestDenyGate_FailedRefreshKeepsTheLastGoodList(t *testing.T) {
	src := &scriptedDenySource{entries: []domain.DenyEntry{{Entry: "203.0.113.0/24"}}}
	g := NewDenyGate(src)
	if err := g.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.entries, src.err = nil, errors.New("worker down")
	if err := g.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh hid the source's failure")
	}
	if _, banned := g.Check(gateOwner, gateAddr); !banned {
		t.Fatal("a failed refresh dropped the last good list")
	}
}

func TestDenyGate_NilGateBansNothing(t *testing.T) {
	var g *DenyGate
	if _, banned := g.Check(gateOwner, gateAddr); banned {
		t.Fatal("a nil gate refused a session")
	}
}

// An expired entry stops matching at its expiry even without a refresh.
func TestDenyGate_ExpiredEntryStopsMatchingWithoutARefresh(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	src := &scriptedDenySource{entries: []domain.DenyEntry{
		{Entry: "203.0.113.0/24", ExpiresAt: now.Add(time.Minute)},
		{Entry: "198.51.100.0/24"},
	}}
	g := NewDenyGate(src)
	g.Now = func() time.Time { return now }
	if err := g.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, banned := g.Check(gateOwner, gateAddr); !banned {
		t.Fatal("live expiring entry did not match")
	}
	src.err = errors.New("worker down")
	now = now.Add(time.Minute)
	if _, banned := g.Check(gateOwner, gateAddr); banned {
		t.Fatal("expired entry still matched")
	}
	if _, banned := g.Check(gateOwner, netip.MustParseAddr("198.51.100.9")); !banned {
		t.Fatal("permanent entry dropped with the expired one")
	}
}
