package domain

import "strings"

// Identity attributes a paste to a user, always shaped as
// `key:<sha256-fingerprint>`. It is both the unit of quota accounting and
// the capability gate for the management verbs.
type Identity string

// IdentityKeyPrefix prefixes the sha256 fingerprint of a presented
// ssh public key.
const IdentityKeyPrefix = "key:"

// IsKeyed reports whether the identity is well-formed. Empty or
// otherwise prefix-less identities are not keyed.
func (i Identity) IsKeyed() bool {
	return strings.HasPrefix(string(i), IdentityKeyPrefix)
}

// String returns the identity as a plain string, the form it is stored in.
func (i Identity) String() string { return string(i) }

// IdentityFromKeyFingerprint wraps a SHA256:... fingerprint in the
// key-identity form.
func IdentityFromKeyFingerprint(fp string) Identity {
	if fp == "" {
		return ""
	}
	return Identity(IdentityKeyPrefix + fp)
}
