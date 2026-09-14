package domain

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
)

// uploadsNamespace is the key root every upload's prefix lives under.
const uploadsNamespace = "uploads/"

// NewUploadID returns a random opaque id naming one upload's object prefix. It
// is minted before any byte is written, so every object the upload stages,
// including what a failed upload leaves behind, sits under a prefix it owns.
func NewUploadID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("hostthis: crypto/rand failure: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}

// ValidUploadID reports whether id is safe to splice into a key: one non-empty
// segment of [A-Za-z0-9_-]. An id read back from metadata must pass before it
// reaches a prefix delete, where an empty id, a dot or a slash would widen the
// delete past its own upload.
func ValidUploadID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// UploadPrefix is the key prefix holding every object of one upload.
func UploadPrefix(id string) string { return uploadsNamespace + id + "/" }

// UploadObjectKey is the key of the n-th file an upload stages.
func UploadObjectKey(id string, n int) string { return UploadPrefix(id) + strconv.Itoa(n) }
