package domain

import (
	"errors"
	"strings"
)

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

// ErrNoOwner refuses an empty or unkeyed identity where an owner is required.
var ErrNoOwner = errors.New("no keyed identity")

// Owner is a keyed identity known to be well-formed, so a service holding one
// needs no anonymity check. The zero value is not valid; use a constructor.
type Owner struct {
	id Identity
}

// OwnerFromKeyFingerprint makes the owner of a presented key's SHA256:...
// fingerprint.
func OwnerFromKeyFingerprint(fp string) (Owner, error) {
	if fp == "" {
		return Owner{}, ErrNoOwner
	}
	return Owner{id: Identity(IdentityKeyPrefix + fp)}, nil
}

// ParseOwner reads an owner from its stored `key:<fingerprint>` form.
func ParseOwner(s string) (Owner, error) {
	fp, ok := strings.CutPrefix(s, IdentityKeyPrefix)
	if !ok {
		return Owner{}, ErrNoOwner
	}
	return OwnerFromKeyFingerprint(fp)
}

// Identity returns the owner's stored identity.
func (o Owner) Identity() Identity { return o.id }

// String returns the stored `key:<fingerprint>` form.
func (o Owner) String() string { return string(o.id) }

// Fingerprint returns the key fingerprint without the `key:` prefix.
func (o Owner) Fingerprint() string {
	return strings.TrimPrefix(string(o.id), IdentityKeyPrefix)
}
