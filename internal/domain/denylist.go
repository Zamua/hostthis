package domain

import (
	"net/netip"
	"regexp"
)

// DenyEntry is one operator denylist entry as stored: a key fingerprint or a
// CIDR prefix (docs/SPEC.md "Operator denylist").
type DenyEntry struct {
	Entry  string
	Reason string
}

var denyKeyShape = regexp.MustCompile(`^SHA256:[0-9a-f]{64}$`)

type denyNet struct {
	prefix netip.Prefix
	reason string
}

// Denylist answers whether a session is banned. The zero value bans nothing.
type Denylist struct {
	keys map[string]string
	nets []denyNet
}

// NewDenylist indexes entries. An entry that is neither shape is skipped, so a
// list written by a newer Worker still loads.
func NewDenylist(entries []DenyEntry) Denylist {
	d := Denylist{keys: map[string]string{}}
	for _, e := range entries {
		if denyKeyShape.MatchString(e.Entry) {
			d.keys[e.Entry] = e.Reason
			continue
		}
		if p, err := netip.ParsePrefix(e.Entry); err == nil {
			d.nets = append(d.nets, denyNet{prefix: p.Masked(), reason: e.Reason})
		}
	}
	return d
}

// Len is the number of usable entries.
func (d Denylist) Len() int { return len(d.keys) + len(d.nets) }

// Match reports whether a session with key fingerprint fp from addr is banned,
// and the matching entry's reason. A key entry is checked first.
func (d Denylist) Match(fp string, addr netip.Addr) (string, bool) {
	if reason, ok := d.keys[fp]; ok {
		return reason, true
	}
	addr = addr.Unmap()
	for _, n := range d.nets {
		if n.prefix.Contains(addr) {
			return n.reason, true
		}
	}
	return "", false
}
