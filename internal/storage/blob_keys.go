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
