package storage

import (
	"errors"
	"fmt"
	"strings"
)

var errInvalidKey = errors.New("blob: invalid object key")

// checkKey accepts a relative, slash-separated key of plain segments, so no
// key read back from metadata can address anything outside the store.
func checkKey(key string) error {
	if key == "" || strings.ContainsAny(key, "\\\x00") {
		return fmt.Errorf("%w: %q", errInvalidKey, key)
	}
	for seg := range strings.SplitSeq(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q", errInvalidKey, key)
		}
	}
	return nil
}

// checkPrefix accepts a key followed by "/", so a prefix delete always ends on
// a segment boundary: "uploads/ab/" never reaches "uploads/abc/".
func checkPrefix(prefix string) error {
	body, ok := strings.CutSuffix(prefix, "/")
	if !ok {
		return fmt.Errorf("%w: prefix %q lacks a trailing slash", errInvalidKey, prefix)
	}
	return checkKey(body)
}

// checkSHA accepts lowercase hex, the only shape a legacy content address has.
func checkSHA(sha string) error {
	if len(sha) < 2 {
		return fmt.Errorf("%w: sha %q", errInvalidKey, sha)
	}
	for i := 0; i < len(sha); i++ {
		if c := sha[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: sha %q", errInvalidKey, sha)
		}
	}
	return nil
}
