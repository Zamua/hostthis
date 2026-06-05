package domain

import (
	"net"
	"strings"
)

// Identity is the string the storage layer uses to attribute a paste
// to a "user." It's either:
//
//   - "key:<sha256-fingerprint>" — the uploader offered an ssh
//     public key
//   - "ip:<subnet>"               — anonymous; the subnet is /24
//     for IPv4 and /48 for IPv6
//
// Identity is the unit of quota accounting: the per-user 1 MiB cap is
// summed by Identity. The capability gates (list, update, delete,
// rename, etc.) only fire when the identity has the "key:" prefix —
// anonymous uploaders are quota-tracked but can't manage anything
// they put up.
type Identity string

const (
	// IdentityKeyPrefix prefixes the sha256 fingerprint of a presented
	// ssh public key. Identities with this prefix can use the
	// management verbs.
	IdentityKeyPrefix = "key:"
	// IdentityIPPrefix prefixes the IP-subnet identifier for anonymous
	// uploaders. Identities with this prefix are quota-tracked but
	// have no management capability.
	IdentityIPPrefix = "ip:"
)

// IsKeyed reports whether the identity belongs to a keyed (managing)
// user. Anonymous identities (ip:...) and empty identities are NOT
// keyed.
func (i Identity) IsKeyed() bool {
	return strings.HasPrefix(string(i), IdentityKeyPrefix)
}

// String returns the identity as a plain string for storage.
func (i Identity) String() string { return string(i) }

// IdentityFromIP builds an anonymous identity from a net.IP, masking
// to /24 (IPv4) or /48 (IPv6). The result is stable across a
// reasonable network change window for the same client.
func IdentityFromIP(ip net.IP) Identity {
	if ip == nil {
		return Identity(IdentityIPPrefix + "unknown")
	}
	if v4 := ip.To4(); v4 != nil {
		// /24 — keep the first 3 octets.
		return Identity(IdentityIPPrefix + v4.Mask(net.CIDRMask(24, 32)).String() + "/24")
	}
	// /48 — keep the first 48 bits (first 3 hex segments).
	return Identity(IdentityIPPrefix + ip.Mask(net.CIDRMask(48, 128)).String() + "/48")
}

// IdentityFromKeyFingerprint wraps a SHA256:... fingerprint in the
// key-identity form.
func IdentityFromKeyFingerprint(fp string) Identity {
	if fp == "" {
		return ""
	}
	return Identity(IdentityKeyPrefix + fp)
}
