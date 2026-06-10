package domain

import "strings"

// Identity is the string the storage layer uses to attribute a paste
// to a user. Always shaped as `key:<sha256-fingerprint>` - every
// hostthis session must offer an ssh public key.
//
// Identity is the unit of quota accounting (the per-user 1 MiB cap
// is summed by Identity) AND the capability gate for the management
// verbs (list, update, delete, etc.).
type Identity string

// IdentityKeyPrefix prefixes the sha256 fingerprint of a presented
// ssh public key.
const IdentityKeyPrefix = "key:"

// IsKeyed reports whether the identity is well-formed. Empty or
// otherwise prefix-less identities are NOT keyed.
func (i Identity) IsKeyed() bool {
	return strings.HasPrefix(string(i), IdentityKeyPrefix)
}

// String returns the identity as a plain string for storage.
func (i Identity) String() string { return string(i) }

// IdentityFromKeyFingerprint wraps a SHA256:... fingerprint in the
// key-identity form.
func IdentityFromKeyFingerprint(fp string) Identity {
	if fp == "" {
		return ""
	}
	return Identity(IdentityKeyPrefix + fp)
}
