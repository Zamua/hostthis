package ssh_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"

	xssh "golang.org/x/crypto/ssh"
)

// genEd25519 wraps crypto/ed25519.GenerateKey to a 2-tuple the test
// caller wants (pub, priv).
func genEd25519() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// fingerprintSigner mirrors server-side fingerprintKey so tests can
// assert "the owner the server captured" without parsing logs.
func fingerprintSigner(pk xssh.PublicKey) string {
	sum := sha256.Sum256(pk.Marshal())
	return "SHA256:" + hex.EncodeToString(sum[:])
}
